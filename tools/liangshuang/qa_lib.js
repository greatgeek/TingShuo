// QA 公共设施：LLM 批调用、隔离区、语料读写、校验正则。各 qa_* 关卡共用。
// 隔离区 quarantine.json：不合格项集中落盘（带 stage/reason），import_notebook.js 跳过隔离项——坏数据永不落库。
const fs = require('fs');
const path = require('path');

const env = {};
for (const line of fs.readFileSync('E:/听说app/TingShuo/backend/.env.dev', 'utf8').split(/\r?\n/)) {
  const m = line.match(/^([A-Z_]+)=(.*)$/);
  if (m) env[m[1]] = m[2].trim();
}
const BASE = env.LLM_BASE_URL.replace(/\/$/, '');
const QFILE = path.join(__dirname, 'quarantine.json');
const OFILE = path.join(__dirname, 'ruby_override.json');

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

async function llm(prompt, tries = 3) {
  for (let t = 0; t < tries; t++) {
    try {
      const r = await fetch(BASE + '/chat/completions', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json', Authorization: 'Bearer ' + env.LLM_API_KEY },
        body: JSON.stringify({ model: env.LLM_MODEL, messages: [{ role: 'user', content: prompt }] }),
      });
      if (!r.ok) throw new Error('HTTP ' + r.status);
      const j = await r.json();
      return JSON.parse(largestJSON(j.choices?.[0]?.message?.content || ''));
    } catch (e) {
      if (t === tries - 1) { console.error('  LLM fail:', e.message); return null; }
      await new Promise(r => setTimeout(r, 1500));
    }
  }
}

async function mapPool(items, size, worker, conc = 4) {
  const chunks = [];
  for (let i = 0; i < items.length; i += size) chunks.push(items.slice(i, i + size));
  const results = new Array(chunks.length);
  let idx = 0;
  const runners = Array.from({ length: conc }, async () => {
    while (true) {
      const my = idx++;
      if (my >= chunks.length) return;
      results[my] = await worker(chunks[my], my);
    }
  });
  await Promise.all(runners);
  return results.flat().filter(Boolean);
}

const loadCorpus = p => JSON.parse(fs.readFileSync(p, 'utf8'));
const saveCorpus = (p, d) => fs.writeFileSync(p, JSON.stringify(d, null, 1));
const loadQ = () => (fs.existsSync(QFILE) ? JSON.parse(fs.readFileSync(QFILE, 'utf8')) : []);
const saveQ = list => fs.writeFileSync(QFILE, JSON.stringify(list, null, 1));
function addQ(list, e) {
  e.at = new Date().toISOString().slice(0, 10);
  if (!list.some(x => x.stage === e.stage && x.reason === e.reason && x.text === e.text && x.surface === e.surface)) list.push(e);
}
const loadOverride = () => (fs.existsSync(OFILE) ? JSON.parse(fs.readFileSync(OFILE, 'utf8')) : {});
const saveOverride = m => fs.writeFileSync(OFILE, JSON.stringify(m, null, 1));

const SIMPLIFIED = /[时给请帮们吗呢把很还从对为务备门变适业让连买卖关开东车马鱼鸟龙风飞见觉现，；“”‘’]/;
const isPureJa = t => !!t && !SIMPLIFIED.test(t) && [...t].length <= 120;
const isZh = t => !!t && /[一-鿯]/.test(t) && !/[ぁ-ん]/.test(t);
const KANA_ONLY = /^[ぁ-ゖァ-ヺー・\d\s　]+$/;

function eachSentence(d, fn) {
  for (const L of d.lessons) for (const c of L.categories) for (const w of c.words) for (const s of w.sentences) fn(L, c, w, s);
}

// 修正校验：拒收含拉丁词/元话语/过短的垃圾修正
const META = /修正|建议|如下|原文|译文|完整句子|应该改为|可以翻译/;
function saneFix(fix, orig) {
  if (!fix || META.test(fix)) return false;
  if (/[A-Za-z]{3,}/.test(fix)) return false;
  if (orig && [...fix].length < Math.max(4, [...orig].length * 0.5)) return false;
  return true;
}

// 译文修正校验：是中文、无假名、不以元话语开头
function saneTrans(fix) {
  if (!fix) return false;
  if (/[ぁ-んァ-ヺ]/.test(fix)) return false;
  if (/^（?(修正|建议|如下|应改)|完整句子/.test(fix)) return false;
  if (/[A-Za-z]{3,}/.test(fix)) return false;
  return true;
}

module.exports = {
  saneTrans,
  saneFix,
  env, BASE, QFILE, OFILE, largestJSON, llm, mapPool,
  loadCorpus, saveCorpus, loadQ, saveQ, addQ, loadOverride, saveOverride,
  SIMPLIFIED, isPureJa, isZh, KANA_ONLY, eachSentence,
};
