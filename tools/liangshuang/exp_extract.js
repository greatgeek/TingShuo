// 表达扩展抽取：从每课「表达扩展」散文段中抽出数量词/日期时间/固定表达，
// 去重后补例句，作为该课第四个目录「表达扩展用单词」合并进 full json。
// 用法: node exp_extract.js <summaryDir> <fullJson>
const fs = require('fs');
const path = require('path');
const env = {};
for (const line of fs.readFileSync('E:/听说app/TingShuo/backend/.env.dev', 'utf8').split(/\r?\n/)) {
  const m = line.match(/^([A-Z_]+)=(.*)$/);
  if (m) env[m[1]] = m[2].trim();
}
const BASE = env.LLM_BASE_URL.replace(/\/$/, '');

function largestJSON(s) {
  let best = '';
  for (const open of ['[', '{']) {
    for (let i = 0; i < s.length; i++) {
      if (s[i] !== open) continue;
      const close = open === '[' ? ']' : '}';
      let depth = 0, inStr = false, esc = false;
      for (let j = i; j < s.length; j++) {
        const c = s[j];
        if (esc) { esc = false; continue; }
        if (inStr) { if (c === '\\') esc = true; else if (c === '"') inStr = false; continue; }
        if (c === '"') inStr = true;
        else if (c === open) depth++;
        else if (c === close) { depth--; if (depth === 0) { const cand = s.slice(i, j + 1); if (cand.length > best.length) { try { JSON.parse(cand); best = cand; } catch {} } break; } }
      }
    }
  }
  return best;
}
async function llm(prompt, tries = 4) {
  for (let t = 0; t < tries; t++) {
    try {
      const ac = new AbortController(); const to = setTimeout(() => ac.abort(), 300000);
      const r = await fetch(BASE + '/chat/completions', { signal: ac.signal,
        method: 'POST',
        headers: { 'Content-Type': 'application/json', Authorization: 'Bearer ' + env.LLM_API_KEY },
        body: JSON.stringify({ model: env.LLM_MODEL, messages: [{ role: 'user', content: prompt }] }),
      });
      clearTimeout(to); if (!r.ok) throw new Error('HTTP ' + r.status);
      const j = await r.json();
      return JSON.parse(largestJSON(j.choices?.[0]?.message?.content || ''));
    } catch (e) { if (t === tries - 1) { console.error('  llm fail:', e.message); return null; } await new Promise(r => setTimeout(r, 1500)); }
  }
}

function sectionExp(md) {
  const i = md.search(/^表达扩展/m);
  if (i < 0) return '';
  const rest = md.slice(i);
  const j = rest.slice(1).search(/^习题用单词/m);
  return (j < 0 ? rest : rest.slice(0, j + 1)).slice(0, 9000);
}

(async () => {
  const dir = process.argv[2], fullFile = process.argv[3];
  const full = JSON.parse(fs.readFileSync(fullFile, 'utf8'));
  // 已有词面（全局去重）
  const existing = new Set();
  for (const L of full.lessons) for (const c of L.categories) for (const w of c.words) existing.add(w.surface);

  const files = fs.readdirSync(dir).filter(f => /^第\d+课\.md$/.test(f))
    .sort((a, b) => Number(a.match(/\d+/)[0]) - Number(b.match(/\d+/)[0]));

  for (const f of files) {
    const lessonNo = Number(f.match(/第(\d+)课/)[1]);
    const md = fs.readFileSync(path.join(dir, f), 'utf8');
    const exp = sectionExp(md);
    if (!exp) { console.log(`第${lessonNo}课: 无表达扩展段`); continue; }
    const L = full.lessons.find(x => x.lesson === lessonNo);
    if (!L) continue;
    if (L.categories.some(c => c.name === '表达扩展用单词')) { console.log(`第${lessonNo}课: 已含表达扩展目录，跳过`); continue; }

    const items = await llm(`下面是日语教材第${lessonNo}课「表达扩展」原文（含交错注音排版）。请从中提取值得单独记忆的语言项目，限三类：
1) 数量词/计数器（如 一つ・二つ・ひとり・ふたり・〜人・〜歳・〜枚・〜杯 等）
2) 日期/时间/星期/年月表达（如 今日・明日・来週・〜月・〜日・曜日・午前午後 等）
3) 本课强调的固定表达/称呼法（如 〜さん・おいくつですか 等短句级表达）
要求：surface 为日语书写形；reading 为全假名（不含音调符号）；meaning 为简明中文；跳过普通名词动词（那些已在常规词表）。每课不超过 25 条。
严格输出 JSON 数组（不要围栏）：[{"surface":"...","reading":"...","meaning":"..."}]
原文：
${exp}`);
    const list = (items || []).filter(x => x && x.surface && x.meaning && !existing.has(x.surface));
    for (const x of list) existing.add(x.surface);
    fs.appendFileSync('E:/听说app/_analysis/exp_progress.log', `第${lessonNo}课 抽取 ${list.length} 条
`);
    if (!list.length) continue;

    // 补例句（每词一句，含中文）
    const sents = {};
    for (let i = 0; i < list.length; i += 15) {
      const batch = list.slice(i, i + 15);
      const lines = batch.map(w => `${w.surface}｜${w.reading || ''}｜${w.meaning}`).join('\n');
      const r = await llm(`为下列每个日语词/表达生成一个自然短句（8—20 字符、包含该项、です・ます体或自然口语），并给完整中文翻译（以句号结尾，不得截断）。
词汇表（surface｜reading｜中文）：
${lines}
严格输出 JSON 数组（不要围栏）：[{"surface":"...","text":"...","translation":"..."}]`);
      for (const g of (r || [])) {
        if (g && g.surface && g.text && g.translation && !/^[。，、,]/.test(g.translation)) {
          (sents[g.surface] = sents[g.surface] || []).push(g);
        }
      }
    }
    L.categories.push({
      name: '表达扩展用单词',
      words: list.map((w, wi) => ({
        surface: w.surface, reading: w.reading || '', pitch: '', pos: '', meaning: w.meaning, level: '',
        sentences: (sents[w.surface] || []).map(g => ({ text: g.text, ruby: [], translation: g.translation, src: 'llm' })),
      })),
    });
  }
  fs.writeFileSync(fullFile, JSON.stringify(full, null, 1));
  let tot = 0;
  for (const L of full.lessons) {
    const c = L.categories.find(x => x.name === '表达扩展用单词');
    if (c) tot += c.words.length;
  }
  fs.appendFileSync('E:/听说app/_analysis/exp_progress.log', `合并完成 总计 ${tot}
`);
})();
