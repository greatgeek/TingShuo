// 为课文行标注说话人（日语表记）与场景（中文），LLM 批量生成后回写 lessons-all.json
// 用法: node annotate_roles.js [lesson,lesson,...]
const fs = require('fs');

const env = {};
for (const line of fs.readFileSync('E:/听说app/TingShuo/backend/.env.dev', 'utf8').split(/\r?\n/)) {
  const m = line.match(/^([A-Z_]+)=(.*)$/);
  if (m) env[m[1]] = m[2].trim();
}
const BASE = env.LLM_BASE_URL.replace(/\/$/, '');
const KEY = env.LLM_API_KEY;
const MODEL = env.LLM_MODEL;

function largestJSON(s) {
  let best = '';
  for (const open of ['[', '{']) {
    const close = open === '[' ? ']' : '}';
    for (let i = 0; i < s.length; i++) {
      if (s[i] !== open) continue;
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
        body: JSON.stringify({ model: MODEL, thinking: { type: 'disabled' }, messages: [{ role: 'user', content: prompt }] }),
        signal: AbortSignal.timeout(120000),
      });
      if (!r.ok) throw new Error('HTTP ' + r.status);
      const j = await r.json();
      return JSON.parse(largestJSON(j.choices?.[0]?.message?.content || ''));
    } catch (e) {
      if (t === tries - 1) { console.error('  LLM fail:', e.message); return null; }
      await new Promise(r => setTimeout(r, 2000));
    }
  }
}

const file = 'out/lessons-all.json';
const data = JSON.parse(fs.readFileSync(file, 'utf8'));
const only = process.argv[2] ? new Set(process.argv[2].split(',').map(Number)) : null;

(async () => {
  for (const L of data.lessons) {
    if (only && !only.has(L.lesson)) continue;
    const need = L.lines.some(l => !Array.isArray(l.speakers) || !l.speakers.length);
    if (!need) continue;
    const listing = L.lines.map((l, i) => `${i + 1}. ${l.text}`).join('\n');
    const prompt = `你是日语教材编辑。下面是「${L.title}」的课文，共 ${L.lines.length} 句，按播放顺序排列。请标注每句话的说话人（用日文表记，如 中村、由加、店員、王、田中、ナレーター）和场景（简体中文，如 在机场的到达大厅）。

规则：
- 说话人只有一个时数组放一个名字；两人合说时放两个；旁白/内心独白用 ナレーター；
- 同一人物全课使用同一表记，不要换名字写法；
- 场景只在段落变化处写具体位置/情境（如 在电器商店、在公司会议室、课堂上），同一场景重复即可；
- 严格对应每一行，行号从 1 到 ${L.lines.length}；
- 只输出 JSON 数组，元素为 {"i": 行号, "speakers": ["..."], "scene": "..."}，不要任何解释。

全文：
${listing}`;
    const arr = await llm(prompt);
    let ok = 0;
    if (!Array.isArray(arr)) { console.log(`第${L.lesson}课 跳过（LLM 失败）`); await new Promise(r => setTimeout(r, 3000)); continue; }
    if (Array.isArray(arr)) {
      for (const it of arr) {
        const idx = Number(it.i) - 1;
        if (!(idx >= 0 && idx < L.lines.length)) continue;
        const sp = Array.isArray(it.speakers) ? it.speakers.filter(x => typeof x === 'string' && x.trim() && x.trim().length <= 12).map(x => x.trim()).slice(0, 3) : [];
        const scene = typeof it.scene === 'string' ? it.scene.trim().slice(0, 40) : '';
        if (!sp.length) continue;
        L.lines[idx].speakers = sp;
        L.lines[idx].scene = scene;
        ok++;
      }
    }
    // 兜底：缺行合并成一次批量请求，仍缺的再逐条补
    let miss = L.lines.map((l, i) => ({ i, l })).filter(x => !Array.isArray(x.l.speakers) || !x.l.speakers.length);
    if (miss.length) {
      const list = miss.map(x => `${x.i + 1}. ${x.l.text}`).join('\n');
      const arr2 = await llm(`下面是日语课文中的若干句子（上课文中的行号与句子）：\n${list}\n请为每一句标注说话人（日文表记）与场景（简体中文）。只输出 JSON 数组：{"i": 行号, "speakers": ["..."], "scene": "..."}。行号必须等于给出的数字。`);
      if (Array.isArray(arr2)) {
        for (const it of arr2) {
          const idx = Number(it.i) - 1;
          if (!(idx >= 0 && idx < L.lines.length)) continue;
          const l = L.lines[idx];
          if (Array.isArray(l.speakers) && l.speakers.length) continue;
          const sp = Array.isArray(it.speakers) ? it.speakers.filter(x => typeof x === 'string' && x.trim() && x.trim().length <= 12).map(x => x.trim()).slice(0, 3) : [];
          if (!sp.length) continue;
          l.speakers = sp;
          l.scene = typeof it.scene === 'string' ? it.scene.trim().slice(0, 40) : '';
          ok++;
        }
      }
      miss = L.lines.filter(l => !Array.isArray(l.speakers) || !l.speakers.length);
      for (const l of miss.slice(0, 8)) {
        const one = await llm(`日语句子：「${l.text}」\n标注这句的说话人与场景。只输出 JSON：{"speakers":["..."],"scene":"..."}。说话人用日文表记。`);
        if (one && Array.isArray(one.speakers) && one.speakers.length) {
          l.speakers = one.speakers.filter(x => typeof x === 'string' && x.trim()).map(x => x.trim()).slice(0, 3);
          l.scene = typeof one.scene === 'string' ? one.scene.trim().slice(0, 40) : (l.scene || '');
          ok++;
        }
      }
    }
    console.log(`第${L.lesson}课 标注 ${ok}/${L.lines.length}`);
    fs.writeFileSync(file, JSON.stringify(data, null, 1));
    await new Promise(r => setTimeout(r, 500));
  }
  const miss = data.lessons.flatMap(L => L.lines.filter(l => !Array.isArray(l.speakers) || !l.speakers.length).map(l => `${L.lesson}:${l.text}`));
  console.log(`剩余未标注 ${miss.length}`);
  miss.forEach(m => console.log('  ' + m));
})();
