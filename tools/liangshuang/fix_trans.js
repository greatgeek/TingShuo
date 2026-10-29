// 修复被截断的译文：重译坏句（小批量+校验+逐条兜底），回写 JSON 并产出 UPDATE SQL。
// 用法: node fix_trans.js <fullJson> <outSql>
const fs = require('fs');
const env = {};
for (const line of fs.readFileSync('E:/听说app/TingShuo/backend/.env.dev', 'utf8').split(/\r?\n/)) {
  const m = line.match(/^([A-Z_]+)=(.*)$/);
  if (m) env[m[1]] = m[2].trim();
}
const BASE = env.LLM_BASE_URL.replace(/\/$/, '');

function bad(text, tr) {
  if (!tr) return 'empty';
  if (/^[。，、,]/.test(tr)) return 'lead-punct';
  if (tr.length < Math.max(4, text.length * 0.25)) return 'too-short';
  if (/。$/.test(text) && !/[。！!？?…]$/.test(tr)) return 'no-end-punct';
  return null;
}
function largestJSON(s) {
  let best = '';
  for (let i = 0; i < s.length; i++) {
    if (s[i] !== '[') continue;
    let depth = 0, inStr = false, esc = false;
    for (let j = i; j < s.length; j++) {
      const c = s[j];
      if (esc) { esc = false; continue; }
      if (inStr) { if (c === '\\') esc = true; else if (c === '"') inStr = false; continue; }
      if (c === '"') inStr = true;
      else if (c === '[') depth++;
      else if (c === ']') { depth--; if (depth === 0) { const cand = s.slice(i, j + 1); if (cand.length > best.length) { try { JSON.parse(cand); best = cand; } catch {} } break; } }
    }
  }
  return best;
}
async function llm(prompt) {
  for (let t = 0; t < 3; t++) {
    try {
      const r = await fetch(BASE + '/chat/completions', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json', Authorization: 'Bearer ' + env.LLM_API_KEY },
        body: JSON.stringify({ model: env.LLM_MODEL, messages: [{ role: 'user', content: prompt }] }),
      });
      if (!r.ok) throw new Error('HTTP ' + r.status);
      const j = await r.json();
      return JSON.parse(largestJSON(j.choices?.[0]?.message?.content || ''));
    } catch (e) { if (t === 2) throw e; await new Promise(r => setTimeout(r, 1200)); }
  }
}
async function translateBatch(batch) {
  const lines = batch.map((t, i) => `${i}. ${t}`).join('\n');
  const prompt = `把下列日语句子翻译成完整、自然的中文。要求：每条译文必须语义完整、以句号/问号/感叹号结尾，不得截断。
严格输出 JSON 数组（不要围栏）：[{"i":0,"t":"完整中文"}...]
${lines}`;
  const r = await llm(prompt);
  const out = {};
  for (const x of (r || [])) {
    if (x && typeof x.i === 'number' && batch[x.i] && !bad(batch[x.i], x.t)) out[batch[x.i]] = x.t;
  }
  return out;
}

(async () => {
  const file = process.argv[2], outSql = process.argv[3];
  const d = JSON.parse(fs.readFileSync(file, 'utf8'));
  const uniq = new Map();
  for (const L of d.lessons) for (const c of L.categories) for (const w of c.words) for (const s of w.sentences) {
    if (!uniq.has(s.text) && bad(s.text, s.translation)) uniq.set(s.text, true);
  }
  const texts = [...uniq.keys()];
  console.log('待修复:', texts.length);
  const fixed = {};
  for (let i = 0; i < texts.length; i += 5) {
    const batch = texts.slice(i, i + 5);
    let res = {};
    try { res = await translateBatch(batch); } catch (e) { console.error('批失败:', e.message); }
    // 仍坏的逐条兜底
    for (const t of batch) {
      if (res[t]) { fixed[t] = res[t]; continue; }
      try {
        const one = await translateBatch([t]);
        if (one[t]) fixed[t] = one[t];
      } catch (e) { console.error('单条失败:', t.slice(0, 16), e.message); }
    }
    console.log(`  进度 ${Math.min(i + 5, texts.length)}/${texts.length} 已修 ${Object.keys(fixed).length}`);
  }
  // 回写 JSON
  let applied = 0;
  for (const L of d.lessons) for (const c of L.categories) for (const w of c.words) for (const s of w.sentences) {
    if (fixed[s.text]) { s.translation = fixed[s.text]; applied++; }
  }
  fs.writeFileSync(file, JSON.stringify(d, null, 1));
  // 产出 UPDATE SQL（dollar quoting 免转义）
  const sql = Object.entries(fixed).map(([t, tr], i) =>
    `UPDATE notebook_sentence SET translation = $ts${i}$${tr}$ts${i}$ WHERE text = $ts${i}$${t}$ts${i}$;`).join('\n');
  fs.writeFileSync(outSql, sql + '\n');
  const still = texts.filter(t => !fixed[t]);
  console.log(`完成: 修复句=${Object.keys(fixed).length} 回写处=${applied} 仍坏=${still.length}`);
  still.forEach(t => console.log('  仍坏:', t.slice(0, 30)));
})();
