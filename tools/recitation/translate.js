// 为课文行补中文译文（按课批量送 LLM，带上下文；校验纯中文后回写）
// 用法: node translate.js out/lessons.json
const fs = require('fs');

const env = {};
for (const line of fs.readFileSync('E:/听说app/TingShuo/backend/.env.dev', 'utf8').split(/\r?\n/)) {
  const m = line.match(/^([A-Z_]+)=(.*)$/);
  if (m) env[m[1]] = m[2].trim();
}
const BASE = env.LLM_BASE_URL.replace(/\/$/, '');
const KEY = env.LLM_API_KEY;
const MODEL = env.LLM_MODEL;
const KANA = /[ぁ-んァ-ヶ]/;
const BAD_START = /^(修正|建议|完整句子|译文|翻译|以上|以下是)/;

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
        else if (c === close) { depth--; if (depth === 0) { const cand = s.slice(i, j + 1); try { JSON.parse(cand); if (cand.length > best.length) best = cand; } catch {} break; } }
      }
    }
  }
  return best;
}

async function llm(prompt, tries = 3) {
  for (let t = 0; t < tries; t++) {
    try {
      const r = await fetch(BASE + '/chat/completions', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json', Authorization: 'Bearer ' + KEY },
        body: JSON.stringify({ model: MODEL, messages: [{ role: 'user', content: prompt }] }),
      });
      if (!r.ok) throw new Error('HTTP ' + r.status);
      const j = await r.json();
      const text = j.choices?.[0]?.message?.content || '';
      return JSON.parse(largestJSON(text));
    } catch (e) {
      if (t === tries - 1) { console.error('  LLM fail:', e.message); return null; }
      await new Promise(r => setTimeout(r, 2000));
    }
  }
}

function sane(zh) {
  if (!zh || typeof zh !== 'string') return '';
  zh = zh.trim().replace(/\s+/g, ' ');
  if (zh.length < 2 || zh.length > 120) return '';
  if (KANA.test(zh)) return '';
  if (BAD_START.test(zh)) return '';
  if ((zh.match(/[A-Za-z]{2,}/g) || []).length > 4) return '';
  return zh;
}

(async () => {
  const file = process.argv[2];
  const data = JSON.parse(fs.readFileSync(file, 'utf8'));
  for (const L of data.lessons) {
    const todo = L.lines.map((l, i) => ({ i, text: l.text })).filter(x => !L.lines[x.i].translation);
    if (!todo.length) continue;
    const listing = todo.map(x => `${x.i + 1}. ${x.text}`).join('\n');
    const prompt = `你是日语教材翻译。下面是《新版中日交流标准日本语》初级上「第${L.lesson}课 ${L.title.replace(/^第\d+课 /, '')}」的会话课文，共 ${L.lines.length} 句，按顺序排列。请为每一句给出自然、口语化的简体中文译文。\n\n要求：\n- 只输出 JSON 数组，元素为 {\"i\": 行号, \"zh\": \"译文\"}，行号与下面编号一致；\n- 译文只含中文与标点，可保留人名商品名等必要外来词（如 Wi-Fi、S、アリペイ→支付宝）；\n- 逐句翻译，不合并、不解释、不加引号；\n- 人名：李由加（りゆか）→ 李由加，中村絵里 → 中村绘里，ジョージ → 乔治，ゆかちゃん → 由加；\n- 数字与金额按中文习惯表达（390円 → 390日元）。\n\n全文：\n${L.lines.map((l, i) => `${i + 1}. ${l.text}`).join('\n')}\n\n只翻译以下待译行：\n${listing}`;
    const arr = await llm(prompt);
    let ok = 0;
    if (Array.isArray(arr)) {
      for (const it of arr) {
        const idx = Number(it.i) - 1;
        if (!(idx >= 0 && idx < L.lines.length)) continue;
        const zh = sane(it.zh);
        if (!zh) { console.log(`  拒收 第${L.lesson}课 ${idx + 1}: ${JSON.stringify(it.zh)}`); continue; }
        L.lines[idx].translation = zh;
        ok++;
      }
    }
    // 兜底：仍未译出的逐条重试
    for (const x of todo) {
      if (L.lines[x.i].translation) continue;
      const one = await llm(`把下面的日语句子翻译成自然的简体中文，只输出译文本身，不要解释：\n${x.text}`);
      const zh = typeof one === 'string' ? sane(one) : '';
      if (zh) { L.lines[x.i].translation = zh; ok++; }
      else console.log(`  失败 第${L.lesson}课 ${x.i + 1}: ${x.text}`);
    }
    console.log(`第${L.lesson}课 译文 ${ok}/${todo.length}`);
    await new Promise(r => setTimeout(r, 500));
  }
  fs.writeFileSync(file, JSON.stringify(data, null, 1));
  const miss = data.lessons.flatMap(L => L.lines.filter(l => !l.translation).map(l => `${L.lesson}:${l.text}`));
  console.log(`剩余缺译 ${miss.length}`);
  miss.forEach(m => console.log('  ' + m));
})();
