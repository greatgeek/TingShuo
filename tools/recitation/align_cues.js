// 教材句子 → SRT 时间轴对齐（DP），输出带时间与低分报告
// 用法: node align_cues.js <srt-all.json> <sources.json> <outLessons.json> [12,13,...]
const fs = require('fs');
const path = require('path');
const { alignLesson } = require('./rec_align_lib');

const srt = JSON.parse(fs.readFileSync(process.argv[2], 'utf8'));
const sources = JSON.parse(fs.readFileSync(process.argv[3], 'utf8'));
const only = process.argv[5] ? new Set(process.argv[5].split(',').map(Number)) : null;

const srtByLesson = new Map();
for (const L of srt) srtByLesson.set(L.part ? `${L.lesson}-${L.part}` : `${L.lesson}`, L.cues);

const lessons = [];
for (const [k, src] of Object.entries(sources)) {
  const lessonNo = Number(k);
  if (only && !only.has(lessonNo)) continue;
  const cues = srtByLesson.get(String(lessonNo));
  if (!cues) { console.error(`第${k}课 无 SRT`); continue; }
  const rows = alignLesson(src.lines, cues);
  const matched = rows.filter(r => r.start != null);
  const low = rows.filter(r => r.start == null || r.score < 0.6);
  lessons.push({
    lesson: lessonNo,
    title: src.title || `第${lessonNo}课`,
    audioKey: `recitation-lesson-${String(lessonNo).padStart(2, '0')}`,
    lines: rows.map(r => ({ start: r.start, end: r.end, text: r.text, translation: r.translation, score: r.score })),
  });
  console.log(`第${k}课 行=${rows.length} 对齐=${matched.length} 低分=${low.length}`);
  for (const r of low) {
    console.log(`  LOW ${r.score.toFixed(2)} ${r.text}\n        ASR: ${(r.spanText || '').slice(0, 80)}`);
  }
}
fs.mkdirSync(path.dirname(process.argv[4]), { recursive: true });
fs.writeFileSync(process.argv[4], JSON.stringify({ lessons }, null, 1));
