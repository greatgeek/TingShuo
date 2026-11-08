// 解析练口语 SRT → JSON（cue 序号、起止秒、文本）
// 用法: node parse_srt.js <videoDir> out/srt.json
const fs = require('fs');
const path = require('path');

function parseTime(s) {
  const m = s.trim().match(/^(\d+):(\d+):(\d+)[,.](\d+)$/);
  if (!m) return NaN;
  return Number(m[1]) * 3600 + Number(m[2]) * 60 + Number(m[3]) + Number(m[4]) / 1000;
}

function parseSrt(text) {
  text = text.replace(/^\uFEFF/, '').replace(/\r/g, '');
  const blocks = text.split(/\n{2,}/);
  const cues = [];
  for (const b of blocks) {
    const lines = b.split('\n').filter(x => x.trim() !== '');
    if (lines.length < 2) continue;
    let i = 0;
    if (/^\d+$/.test(lines[0].trim())) i = 1;
    const tm = (lines[i] || '').match(/^(.+?)\s*-->\s*(.+)$/);
    if (!tm) continue;
    const start = parseTime(tm[1]), end = parseTime(tm[2]);
    const body = lines.slice(i + 1).join(' ').replace(/\s+/g, ' ').trim();
    if (!body || !isFinite(start) || !isFinite(end)) continue;
    cues.push({ start: Math.round(start * 1000) / 1000, end: Math.round(end * 1000) / 1000, text: body });
  }
  return cues;
}

const dir = process.argv[2];
const out = process.argv[3];
const files = fs.readdirSync(dir).filter(f => /练口语.*\.srt$/i.test(f));
const result = [];
for (const f of files) {
  const m = f.match(/【0?(\d+)练口语】/);
  if (!m) continue;
  const lesson = Number(m[1]);
  const cues = parseSrt(fs.readFileSync(path.join(dir, f), 'utf8'));
  result.push({ lesson, file: f, cues });
}
result.sort((a, b) => a.lesson - b.lesson);
fs.mkdirSync(path.dirname(out), { recursive: true });
fs.writeFileSync(out, JSON.stringify(result, null, 1));
for (const L of result) {
  console.log(`第${L.lesson}课 ${L.cues.length} cue, ${L.cues[L.cues.length - 1].end}s`);
  L.cues.forEach((c, i) => console.log(`  ${i + 1}\t${c.start}-${c.end}\t${c.text}`));
}
