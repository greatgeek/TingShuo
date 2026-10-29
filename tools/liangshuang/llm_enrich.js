// LLM 补全：① 无句词批量生成例句+译文 ② 会话句批量补中文译文。
// 用法: node llm_enrich.js <inJson> <outJson>
const fs = require('fs');

// 读本地 .env.dev 的 LLM 配置
const env = {};
for (const line of fs.readFileSync('E:/听说app/TingShuo/backend/.env.dev', 'utf8').split(/\r?\n/)) {
  const m = line.match(/^([A-Z_]+)=(.*)$/);
  if (m) env[m[1]] = m[2].trim();
}
const BASE = env.LLM_BASE_URL.replace(/\/$/, '');
const KEY = env.LLM_API_KEY;
const MODEL = env.LLM_MODEL;

function largestJSON(s) {
  // 取最大的平衡 [ ] 或 { } 块
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

async function llm(prompt, tries = 2) {
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
      const parsed = JSON.parse(largestJSON(text));
      return parsed;
    } catch (e) {
      if (t === tries - 1) { console.error('  LLM fail:', e.message); return null; }
      await new Promise(r => setTimeout(r, 1500));
    }
  }
}

async function mapPool(items, size, worker) {
  const out = [];
  for (let i = 0; i < items.length; i += size) {
    out.push(items.slice(i, i + size));
  }
  const results = new Array(out.length);
  let idx = 0;
  const runners = Array.from({ length: 4 }, async () => {
    while (true) {
      const my = idx++;
      if (my >= out.length) return;
      results[my] = await worker(out[my], my);
    }
  });
  await Promise.all(runners);
  return results.flat().filter(Boolean);
}

(async () => {
  const inp = process.argv[2], outp = process.argv[3];
  const data = JSON.parse(fs.readFileSync(inp, 'utf8'));

  // ① 无句词 → 生成例句
  const need = [];
  const seen = new Set();
  for (const L of data.lessons) for (const c of L.categories) for (const w of c.words) {
    if (!w.sentences.length && !seen.has(w.surface)) { seen.add(w.surface); need.push(w); }
  }
  console.log('无句词(去重):', need.length);
  const gen = await mapPool(need, 20, async (batch, bi) => {
    const lines = batch.map(w => `${w.surface}｜${w.reading || ''}｜${w.meaning}`).join('\n');
    const prompt = `你是日语教学专家。为下列每个词生成一个自然短句（8—20 字符、包含该词、です・ます体或自然短语），并给自然中文翻译。
词汇表（surface｜reading｜中文）：
${lines}
严格输出 JSON 数组（不要围栏）：[{"surface":"...","text":"...","translation":"..."}]`;
    const r = await llm(prompt);
    if (!r) return [];
    console.log(`  词批 ${bi + 1}: ${r.length}/${batch.length}`);
    return r.filter(x => x && x.surface && x.text);
  });
  const genBySurface = {};
  for (const g of gen) (genBySurface[g.surface] = genBySurface[g.surface] || []).push(g);

  // ② 会话句补译文：收集唯一无译文句
  const convSet = new Map();
  for (const L of data.lessons) for (const c of L.categories) for (const w of c.words) for (const s of w.sentences) {
    if (!s.translation && !convSet.has(s.text)) convSet.set(s.text, true);
  }
  const convTexts = [...convSet.keys()];
  console.log('需补译句(去重):', convTexts.length);
  // 译文批处理：worker 内把批内索引映射回原文，避免扁平化丢失批次信息
  const trans = await mapPool(convTexts, 20, async (batch) => {
    const lines = batch.map((t, i) => `${i}. ${t}`).join('\n');
    const prompt = `把下列日语句子翻译成自然中文（保留语气），严格输出 JSON 数组（不要围栏）：[{"i":0,"t":"中文"}...]
${lines}`;
    const r = await llm(prompt);
    if (!r) return [];
    console.log(`  译批: ${r.length}/${batch.length}`);
    return r.filter(x => x && typeof x.i === 'number' && batch[x.i]).map(x => ({ text: batch[x.i], t: x.t }));
  });
  const transByText = {};
  for (const x of trans) transByText[x.text] = x.t;

  // 合并
  let filledWords = 0, filledTrans = 0;
  for (const L of data.lessons) for (const c of L.categories) for (const w of c.words) {
    if (!w.sentences.length && genBySurface[w.surface]) {
      w.sentences = genBySurface[w.surface].map(g => ({ text: g.text, ruby: [], translation: g.translation || '', src: 'llm' }));
      filledWords++;
    }
    for (const s of w.sentences) {
      if (!s.translation && transByText[s.text]) { s.translation = transByText[s.text]; filledTrans++; }
    }
  }
  fs.writeFileSync(outp, JSON.stringify(data, null, 1));
  let still0 = 0, stillNoTrans = 0;
  const uniq0 = new Set();
  for (const L of data.lessons) for (const c of L.categories) for (const w of c.words) {
    if (!w.sentences.length && !uniq0.has(w.surface)) { uniq0.add(w.surface); still0++; }
    for (const s of w.sentences) if (!s.translation) stillNoTrans++;
  }
  console.log(`完成: 补句词=${filledWords} 补译句=${filledTrans} 仍无句词=${still0} 仍无译例句=${stillNoTrans}`);
})();
