// 听力训练内容 JSON → 导入 SQL。按 lesson id 整课重建（progress 会随级联删除，本地重跑可接受）。
// 用法：node build_sql.js [lessonNo]  → out/import_listening_<NN>.sql
const fs = require('fs');
const path = require('path');

const dir = path.join(__dirname, 'lessons');
const only = process.argv[2] ? String(parseInt(process.argv[2], 10)).padStart(2, '0') : null;
const files = fs.readdirSync(dir).filter(f => f.endsWith('.json') && (!only || f === `l${only}.json`));
if (!files.length) { console.error('没有匹配的课程 JSON'); process.exit(1); }

const q = s => `'${String(s).replace(/'/g, "''")}'`;
const j = v => `'${JSON.stringify(v).replace(/'/g, "''")}'::jsonb`;

function buildLesson(l) {
  const p = 'lsn' + String(l.lesson_no).padStart(2, '0');
  const out = [];
  out.push(`DELETE FROM listening_lesson WHERE id = ${q(l.id)};`);
  out.push(`INSERT INTO listening_lesson (id, lesson_no, title, audio_key, duration_ms, sort_order) VALUES (${[
    q(l.id), l.lesson_no, q(l.title), q(l.audio_key), l.duration_ms, l.sort_order || 0,
  ].join(', ')});`);

  for (const s of l.sections) {
    const sid = `${p}-s${s.section_no}`;
    out.push(`INSERT INTO listening_section (id, lesson_id, section_no, kind, instruction, audio_start_ms, audio_end_ms, example_prompt, example_answer) VALUES (${[
      q(sid), q(l.id), s.section_no, q(s.kind), q(s.instruction),
      s.audio_start_ms, s.audio_end_ms,
      q(s.example ? s.example.prompt : ''), q(s.example ? s.example.answer : ''),
    ].join(', ')});`);
    (s.questions || []).forEach((item, i) => {
      out.push(`INSERT INTO listening_question (id, section_id, question_no, prompt, options, answer) VALUES (${[
        q(`${sid}-q${i + 1}`), q(sid), i + 1, q(item.prompt), j(item.options || []), q(item.answer),
      ].join(', ')});`);
    });
    (s.lines || []).forEach((ln, i) => {
      out.push(`INSERT INTO listening_line (id, section_id, line_no, speaker, start_ms, end_ms, text, translation) VALUES (${[
        q(`${sid}-l${i + 1}`), q(sid), i + 1, q(ln.speaker), ln.start_ms, ln.end_ms,
        q(ln.text), q(ln.translation),
      ].join(', ')});`);
    });
  }
  return out.join('\n');
}

const outDir = path.join(__dirname, 'out');
fs.mkdirSync(outDir, { recursive: true });
for (const f of files) {
  const l = JSON.parse(fs.readFileSync(path.join(dir, f), 'utf8'));
  const sql = '-- 听力训练导入（build_sql.js 生成）\nBEGIN;\n' + buildLesson(l) + '\nCOMMIT;\n';
  const target = path.join(outDir, `import_listening_${String(l.lesson_no).padStart(2, '0')}.sql`);
  fs.writeFileSync(target, sql, 'utf8');
  const qn = l.sections.reduce((n, s) => n + (s.questions || []).length, 0);
  const ln = l.sections.reduce((n, s) => n + (s.lines || []).length, 0);
  console.log(`${target}: 大题=${l.sections.length} 题=${qn} 句=${ln}`);
}