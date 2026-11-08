// 从初级上 _summary/第N课.md 的「会话原文」抽取干净课文对话（去交错振り仮名、说话人前缀、舞台提示）
// 用法: node extract_dialogue.js <summaryDir> <outJson> [5,6,7,8,9]
const fs = require('fs');
const path = require('path');

const KANJI = '[㐀-䶿一-鿿]';
const reKanji = new RegExp(KANJI);
const reKana = /^[ぁ-んァ-ヶー]+$/;
const KEEP = new Set(['は','が','を','に','で','と','も','へ','か','よ','ね','さ','て','わ','な','の','や','ぞ','ぜ','ば','っ',
  'です','ます','でした','ました','でしょ','でしょう','ください','ましょう','ながら','ても','しか','まで','だけ','ほど','くらい','ぐらい','など',
  'た','て','る','れ','ら','ない','ある','いる','する','くる','きます','きました','します','しました','たい','だ','じゃ','って','ても','から','ので']);

function cleanJP(raw) {
  raw = raw.replace(/([(（][^)）]*[)）])/g, ' $1 ');
  const tokens = raw.match(/[(（][^)）]*[)）]|\S+/g) || [];
  const out = [];
  let prevKanji = null;
  for (let i = 0; i < tokens.length; i++) {
    const tok = tokens[i];
    if (tok.startsWith('（') || tok.startsWith('(')) {
      const inner = tok.slice(1, -1);
      if (inner.trim() === '') { prevKanji = null; continue; }
      if (reKanji.test(inner)) { prevKanji = null; continue; }
      const m = inner.match(/^([ぁ-んァ-ヶー]+)([\s\S]*)$/);
      if (m && m[2].trim()) out.push(m[2].trim());
      prevKanji = null;
      continue;
    }
    if (reKana.test(tok)) {
      const next = tokens[i + 1] || '';
      const nextIsKanji = reKanji.test(next.replace(/[(（][^)）]*[)）]/g, ''));
      if (prevKanji && !KEEP.has(tok)) { prevKanji = null; continue; }
      if (!prevKanji && nextIsKanji && !KEEP.has(tok) && tok.length >= 2) { prevKanji = null; continue; }
      out.push(tok); prevKanji = null; continue;
    }
    out.push(tok);
    const m = tok.match(new RegExp('(' + KANJI + '+)$'));
    prevKanji = m ? m[1] : null;
  }
  return out.join('');
}

function conversationSection(md) {
  let s = md.search(/^##\s*会话原文\s*$/m);
  if (s < 0) s = md.search(/^\s*(?:会\s*话|课\s*文)\s*$/m);
  if (s < 0) return '';
  const rest = md.slice(s);
  const e = rest.slice(1).search(/^\s*(?:###\s*会话用单词|###\s*语法解释用单词|课文(?:用)?单词)\s*$/m);
  return e < 0 ? rest : rest.slice(0, e + 1);
}

const dir = process.argv[2];
const out = process.argv[3];
const wanted = (process.argv[4] || '5,6,7,8,9').split(',').map(Number);
const result = {};
for (const n of wanted) {
  const md = fs.readFileSync(path.join(dir, `第${n}课.md`), 'utf8');
  const conv = conversationSection(md);
  const lines = [];
  for (const raw of conv.split('\n')) {
    let t = raw.replace(/\r/g, '').trim();
    if (!t || /^##/.test(t) || /^（/.test(t)) continue;
    // 说话人前缀：由（ゆ）加（か）：/ 中村：/ ジョージ： —— 去掉前缀保留台词
    t = t.replace(/^[^：:]{1,20}[：:]\s*/, '');
    t = t.replace(/^[（(][^）)]*[）)]\s*/, '');
    if (!t) continue;
    const text = cleanJP(t);
    if (!text || (text.match(/[ぁ-んァ-ヶ]/g) || []).length < 2) continue;
    lines.push(text);
  }
  result[n] = lines;
}
fs.mkdirSync(path.dirname(out), { recursive: true });
fs.writeFileSync(out, JSON.stringify(result, null, 1));
for (const [n, ls] of Object.entries(result)) {
  console.log(`=== 第${n}课 (${ls.length}行) ===`);
  ls.forEach((l, i) => console.log(`${i + 1}. ${l}`));
}
