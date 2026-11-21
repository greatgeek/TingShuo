// 组装 30 课课文本数据：教材对齐（12-42）+ 音频重建（43-45），第40课两段合并
// 用法: node assemble_lessons.js
// 输入: out/srt-all.json, out/sources.json, out/titles.json, out/srt-lines-gj.json, out/audio-meta.json(可选)
// 输出: out/lessons-all.json, out/lessons-review.txt
const fs = require('fs');
const path = require('path');
const { alignLesson } = require('./rec_align_lib');

const OUT = path.join(__dirname, 'out');
const read = f => JSON.parse(fs.readFileSync(path.join(OUT, f), 'utf8'));
const readIf = f => (fs.existsSync(path.join(OUT, f)) ? read(f) : null);

const srt = read('srt-all.json');
const sources = read('sources.json');
const titles = read('titles.json');
const gjLines = readIf('srt-lines-gj.json') || {};
const longtext = readIf('longtext-lines.json') || {};
const meta = readIf('audio-meta.json') || {};

const SCOPE = (process.argv[2] ? process.argv[2].split(',').map(Number) : [...Array(10)].map((_, i) => 12 + i)
  .concat([...Array(10)].map((_, i) => 23 + i))
  .concat([...Array(10)].map((_, i) => 36 + i)));

const srtByLesson = new Map();
for (const L of srt) srtByLesson.set(L.part ? `${L.lesson}-${L.part}` : `${L.lesson}`, L.cues);

const cleanTitle = t => {
  let s = String(t || '');
  if (/[（(](前編|後編)[）)]/.test(s)) return s;
  for (let i = 0; i < 3; i++) s = s.replace(/[（(]([^（）()]*)[）)]/g, '$1');
  return s.trim();
};
const titleOf = (n, raw) => {
  let t = cleanTitle(raw);
  if (n >= 43 && n <= 45 && t === '和菓子に見る日本の美意識') t += ['（一）', '（二）', '（三）'][n - 43];
  return `第${n}课 ${t}`.trim();
};

const lessons = [];
const review = [];
for (const n of SCOPE) {
  let lines = [];
  if (gjLines[n]) {
    lines = gjLines[n].lines
      .filter(l => !/^(はい|うん|そう|あ|ええ|まあ|そうですね)[。、！]?$/.test(l.text))
      .map(l => ({ start: l.start, end: l.end, text: l.text, translation: l.translation || '', score: 1 }));
  } else if (n === 40) {
    const all = (longtext[40] || (sources[40] && sources[40].lines) || []);
    const cues1 = srtByLesson.get('40-1'), cues2 = srtByLesson.get('40-2');
    const offset = meta['40-1'] || (cues1[cues1.length - 1].end + 0.3);
    if (all.some(l => l.part)) {
      const rows1 = alignLesson(all.filter(l => l.part !== 2), cues1);
      const rows2 = alignLesson(all.filter(l => l.part === 2), cues2);
      const kept1 = rows1.filter(r => r.start != null);
      const kept2 = rows2.filter(r => r.start != null).map(r => ({ ...r, start: Math.round((r.start + offset) * 1000) / 1000, end: Math.round((r.end + offset) * 1000) / 1000 }));
      lines = [...kept1, ...kept2];
      review.push(`第40课: 段1对齐=${kept1.length} 段2对齐=${kept2.length}`);
      for (const r of rows1.filter(r => r.start == null)) review.push(`  40 未匹配(段1): ${r.text}`);
      for (const r of rows2.filter(r => r.start == null)) review.push(`  40 未匹配(段2): ${r.text}`);
    } else {
      const rows1 = alignLesson(all, cues1);
      const kept1 = [];
      const remain = [];
      rows1.forEach((r, i) => {
        if (r.start != null) kept1.push({ ...r, part: 1 });
        else remain.push(all[i]);
      });
      const rows2 = alignLesson(remain, cues2);
      const kept2 = rows2.filter(r => r.start != null).map(r => ({ ...r, part: 2 }));
      for (const l of kept2) { l.start = Math.round((l.start + offset) * 1000) / 1000; l.end = Math.round((l.end + offset) * 1000) / 1000; }
      lines = [...kept1, ...kept2];
      review.push(`第40课: 段1对齐=${kept1.length} 段2对齐=${kept2.length} 未匹配=${remain.length - kept2.length}`);
      for (const r of rows1.filter(r => r.start == null)) review.push(`  40 未匹配(段1/2): ${r.text}`);
      for (const r of rows2.filter(r => r.start == null)) review.push(`  40 未匹配(段2): ${r.text}`);
    }
  } else {
    const src = longtext[n] ? { lines: longtext[n] } : sources[n];
    if (!src) { review.push(`第${n}课: 缺教材句源`); continue; }
    const cues = srtByLesson.get(String(n));
    if (!cues) { review.push(`第${n}课: 缺 SRT`); continue; }
    const rows = alignLesson(src.lines, cues);
    for (const r of rows) {
      if (r.start == null) review.push(`第${n}课 未匹配: ${r.text}${r.spanText ? ' | ASR:' + r.spanText.slice(0, 40) : ''}`);
    }
    lines = rows.filter(r => r.start != null);
  }
  // 去掉连续重复句
  lines = lines.filter((l, i) => i === 0 || l.text !== lines[i - 1].text);
  const bad = lines.filter(l => l.start == null);
  lessons.push({
    lesson: n,
    title: titleOf(n, titles[n]),
    audioKey: `recitation-lesson-${String(n).padStart(2, '0')}`,
    durationSec: meta[String(n)] || (n === 40 ? (meta['40-1'] || 0) + (meta['40-2'] || 0) : undefined),
    lines: lines.map(l => ({ start: l.start, end: l.end, text: l.text, translation: l.translation || '', score: l.score })),
  });
  console.log(`第${n}课 行=${lines.length}${bad.length ? ' BAD=' + bad.length : ''}`);
}
const outFile = process.argv[2] ? 'lessons-subset.json' : 'lessons-all.json';
fs.writeFileSync(path.join(OUT, outFile), JSON.stringify({ lessons }, null, 1));
fs.writeFileSync(path.join(OUT, 'lessons-review.txt'), review.join('\n') + '\n');
console.log(`课数=${lessons.length} 总行=${lessons.reduce((s, L) => s + L.lines.length, 0)}`);
console.log(`低匹配明细见 out/lessons-review.txt（${review.length} 条）`);
