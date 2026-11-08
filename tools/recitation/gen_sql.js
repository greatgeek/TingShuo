// 生成课文本导入 SQL（幂等 upsert，不删除行——保留用户进度）
// 用法: node gen_sql.js out/lessons.json out/import_recitation.sql
const fs = require('fs');

const DURATION_MS = {
  5: 77736,
  6: 53928,
  7: 74928,
  8: 57144,
  9: 55848,
};

function q(s) {
  return "'" + String(s).replace(/'/g, "''") + "'";
}
function ms(sec) {
  return Math.round(sec * 1000);
}

const data = JSON.parse(fs.readFileSync(process.argv[2], 'utf8'));
const out = [];
out.push('-- 课文本导入：练口语第5—9课（幂等 upsert，重跑不会丢背诵/听写进度）');
out.push('BEGIN;');
for (const L of data.lessons) {
  const id = `rec-${String(L.lesson).padStart(2, '0')}`;
  out.push(`INSERT INTO recitation_lesson (id, lesson_no, title, audio_key, duration_ms, sort_order) VALUES (` +
    `${q(id)}, ${L.lesson}, ${q(L.title)}, ${q(L.audioKey)}, ${DURATION_MS[L.lesson]}, ${L.lesson}) ` +
    `ON CONFLICT (id) DO UPDATE SET lesson_no=EXCLUDED.lesson_no, title=EXCLUDED.title, ` +
    `audio_key=EXCLUDED.audio_key, duration_ms=EXCLUDED.duration_ms, sort_order=EXCLUDED.sort_order;`);
  for (let i = 0; i < L.lines.length; i++) {
    const l = L.lines[i];
    const lineNo = i + 1;
    const lid = `recl-${String(L.lesson).padStart(2, '0')}-${String(lineNo).padStart(2, '0')}`;
    out.push(`INSERT INTO recitation_line (id, lesson_id, line_no, start_ms, end_ms, text, translation) VALUES (` +
      `${q(lid)}, ${q(id)}, ${lineNo}, ${ms(l.start)}, ${ms(l.end)}, ${q(l.text)}, ${q(l.translation)}) ` +
      `ON CONFLICT (lesson_id, line_no) DO UPDATE SET start_ms=EXCLUDED.start_ms, end_ms=EXCLUDED.end_ms, ` +
      `text=EXCLUDED.text, translation=EXCLUDED.translation;`);
  }
}
out.push('COMMIT;');
fs.writeFileSync(process.argv[3], out.join('\n') + '\n');
console.log(`SQL 行数=${out.length}, 课数=${data.lessons.length}, 行数=${data.lessons.reduce((n, L) => n + L.lines.length, 0)}`);
