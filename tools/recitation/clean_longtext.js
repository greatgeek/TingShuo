// 长文课文（本文/日记/演讲稿）按句还原：去振假名、补译文、切句，供课文本对齐
// 用法: node clean_longtext.js [27,29,38,40,41,42]
// 输出: out/longtext-lines.json  { "27": [{text,translation}], ... }
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

async function llm(prompt, tries = 4) {
  for (let t = 0; t < tries; t++) {
    try {
      const r = await fetch(BASE + '/chat/completions', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json', Authorization: 'Bearer ' + KEY },
        body: JSON.stringify({ model: MODEL, messages: [{ role: 'user', content: prompt }] }),
      });
      if (!r.ok) throw new Error('HTTP ' + r.status);
      const j = await r.json();
      return JSON.parse(largestJSON(j.choices?.[0]?.message?.content || ''));
    } catch (e) {
      if (t === tries - 1) { console.error('  LLM fail:', e.message); return null; }
      await new Promise(r => setTimeout(r, 2500));
    }
  }
}

const PROMPT = raw => `下面是一课日语教材的「本文」（说明文/日记/演讲稿）原始转写（来自 PDF）。振假名与正文交错，PDF 转换常把右括号错放到句尾，使括号里混进正文。例如：
粉（しょう） 症 の季 き せつ 節（もやってきます。）→ 花粉症の季節もやってきます。
请还原干净、完整的日文，并按句切分：
1. 去掉所有振假名标注；被括号吞掉的正文按原顺序接回；跨行断开的词拼接完整；不要改写措辞、不要增删内容。
2. 按句切分：每句以 。？！ 结尾为一条；一句太长（超过 60 字）可在 で、が、から 等自然停顿处再切，保持原词序与句尾。
3. 跳过无实质内容的部分（页码、标题、作者信息、括号内舞台说明、纯中文）。
4. 每条给自然、完整的中文翻译。
严格输出 JSON 数组（不要围栏）：[{"text":"干净日文","translation":"中文"}]

原文：
${raw}`;

const KANA = /[ぁ-んァ-ヶ]/;
const KANJI = /[一-龯]/;
const ZH_BAD = /^(以下|译文|翻译|注|说明)/;

const files = {
  25: '../liangshuang/out/chuji-xia-full.json',
  27: '../liangshuang/out/zhong-shang.json',
  29: '../liangshuang/out/zhong-shang.json',
  38: '../liangshuang/out/zhong-xia.json',
  40: '../liangshuang/out/gaoji.json',
  41: '../liangshuang/out/gaoji.json',
  42: '../liangshuang/out/gaoji.json',
};
const only = process.argv[2] ? new Set(process.argv[2].split(',').map(Number)) : null;

const outFile = 'out/longtext-lines.json';
const out = fs.existsSync(outFile) ? JSON.parse(fs.readFileSync(outFile, 'utf8')) : {};

(async () => {
  for (const [n, file] of Object.entries(files)) {
    const lesson = Number(n);
    if (only && !only.has(lesson)) continue;
    const d = JSON.parse(fs.readFileSync(file, 'utf8'));
    const L = d.lessons.find(x => x.lesson === lesson);
    if (!L) { console.error(`缺第${lesson}课`); continue; }
    const raw = String((L.raw || {}).conversation || '').replace(/^#.*$/gm, '').trim().slice(0, 20000);
    const arr = await llm(PROMPT(raw));
    if (!Array.isArray(arr)) { console.error(`第${lesson}课 还原失败`); continue; }
    const lines = [];
    for (const it of arr) {
      const text = String(it.text || '').replace(/\s+/g, '').trim();
      let zh = String(it.translation || '').trim().replace(/\s+/g, ' ');
      if (text.length < 4 || text.length > 300) continue;
      if (!KANA.test(text) || !KANJI.test(text)) continue;
      if (KANA.test(zh) || zh.length < 2 || ZH_BAD.test(zh)) zh = '';
      lines.push({ text, translation: zh });
    }
    out[lesson] = lines;
    fs.writeFileSync(outFile, JSON.stringify(out, null, 1));
    console.log(`第${lesson}课 行=${lines.length} 缺译=${lines.filter(l => !l.translation).length}`);
    await new Promise(r => setTimeout(r, 500));
  }
  console.log('写出', outFile);
})();
