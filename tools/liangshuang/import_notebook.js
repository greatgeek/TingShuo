// 生词本导入（增量）：只重建本教材的顶层目录子树，不删 notebook、不动其他教材。
// 用法: node import_notebook.js <json> <userId> <outSql>
const fs = require('fs');
const data = JSON.parse(fs.readFileSync(process.argv[2], 'utf8'));
const UID = process.argv[3];
const NB = 'nb-liangshuang';
const LEVEL = data.level1 || '初级上';
const ROOT_DEF = {
  '初级上': ['nbd-cjs', 0],
  '初级下': ['nbd-cjs-xia', 1],
  '中级上': ['nbd-cjs-zs', 2],
  '中级下': ['nbd-cjs-zx', 3],
  '高级': ['nbd-cjs-gj', 4],
};
const [ROOT, ROOT_SORT] = ROOT_DEF[LEVEL] || ROOT_DEF['初级上'];
const esc = s => String(s ?? '').replace(/'/g, "''");

// 隔离区（各 qa_* 关产出）：不合格句/词不落库
const qf = require('path').join(__dirname, 'quarantine.json');
const qlist = fs.existsSync(qf) ? JSON.parse(fs.readFileSync(qf, 'utf8')) : [];
const SKIP_TEXT = new Set(qlist.filter(x => x.text && ['bind', 'dup', 'trans', 'trans-empty', 'grammar', 'truncated', 'mixed', 'typo', 'ruby-struct', 'audio-block'].includes(x.reason)).map(x => x.text));
const SKIP_WORD = new Set(qlist.filter(x => x.surface && ['reading', 'hallucinate'].includes(x.reason)).map(x => x.surface));
let skippedWords = 0, skippedSents = 0;

// 词去重（surface|reading），保留首个非空 pos/meaning；同形不同音调按教材分列，合并标注
const stripPos = s => s.replace(/^\[[^\]]*\]\s*·\s*/, '');
const words = new Map();
const links = [];   // {dirId, surface, reading, sort}
const sents = [];   // {linkKey, text, translation, src}
for (const L of data.lessons) {
  const ln = String(L.lesson).padStart(2, '0');
  const ldir = `${ROOT}-L${ln}`;
  L.categories.forEach((c, ci) => {
    const cdir = `${ldir}-c${ci}`;
    c.words.forEach((w, wi) => {
      if (SKIP_WORD.has(w.surface)) { skippedWords++; return; }
      const reading = w.reading || '';
      const key = w.surface + '|' + reading;
      if (!words.has(key)) words.set(key, { surface: w.surface, reading, pos: w.pos || '', meaning: w.meaning || '', pitch: w.pitch || '' });
      else {
        const e = words.get(key);
        if (!e.pos && w.pos) e.pos = w.pos;
        if (!e.meaning && w.meaning) e.meaning = w.meaning;
        if (e.pitch && w.pitch && e.pitch !== w.pitch && e.meaning && w.meaning
            && !e.meaning.includes(w.pitch + ' ')) {
          const posPrefix = (e.meaning.match(/^(\[[^\]]*\]\s*·\s*)/) || w.meaning.match(/^(\[[^\]]*\]\s*·\s*)/) || [])[1] || '';
          e.meaning = `${posPrefix}${e.pitch} ${stripPos(e.meaning)}；${w.pitch} ${stripPos(w.meaning)}`;
        }
      }
      const linkKey = `${cdir}::${key}`;
      links.push({ dirId: cdir, surface: w.surface, reading, sort: wi, linkKey });
      (w.sentences || []).forEach((s, si) => {
        if (SKIP_TEXT.has(s.text)) { skippedSents++; return; }
        sents.push({ linkKey, text: s.text, translation: s.translation || '', src: s.src || '', sort: si });
      });
    });
  });
}

const out = [];
// 只重建本教材的顶层目录（级联清其下目录/挂载/例句），不动 notebook 与其他教材
out.push(`DELETE FROM notebook_dir WHERE id = '${ROOT}';`);
out.push(`INSERT INTO notebook (id, user_id, name) SELECT '${NB}', '${UID}', '梁爽日语课' WHERE NOT EXISTS (SELECT 1 FROM notebook WHERE user_id = '${UID}' AND name = '梁爽日语课') ON CONFLICT DO NOTHING;`);
out.push(`INSERT INTO notebook_dir (id, notebook_id, parent_id, name, sort_order) SELECT '${ROOT}', id, NULL, '${LEVEL}', ${ROOT_SORT} FROM notebook WHERE user_id = '${UID}' AND name = '梁爽日语课';`);
for (const L of data.lessons) {
  const ln = String(L.lesson).padStart(2, '0');
  out.push(`INSERT INTO notebook_dir (id, notebook_id, parent_id, name, sort_order) SELECT '${ROOT}-L${ln}', id, '${ROOT}', '第${L.lesson}课', ${L.lesson} FROM notebook WHERE user_id = '${UID}' AND name = '梁爽日语课';`);
  L.categories.forEach((c, ci) => {
    out.push(`INSERT INTO notebook_dir (id, notebook_id, parent_id, name, sort_order) SELECT '${ROOT}-L${ln}-c${ci}', id, '${ROOT}-L${ln}', '${esc(c.name)}', ${ci} FROM notebook WHERE user_id = '${UID}' AND name = '梁爽日语课';`);
  });
}
for (const [key, w] of words) {
  out.push(`INSERT INTO word (id, user_id, surface, reading, pos, meaning) VALUES ('w-' || substr(md5('${esc(key)}'),1,24), '${UID}', '${esc(w.surface)}', '${esc(w.reading)}', '${esc(w.pos)}', '${esc(w.meaning)}') ON CONFLICT (user_id, surface, reading) DO NOTHING;`);
}
// linkKey → 确定性 notebook_word id（md5，避免日文字符被滤空撞 id）
const crypto = require('crypto');
const linkId = k => 'nbw-' + crypto.createHash('md5').update(k).digest('hex').slice(0, 24);
for (const l of links) {
  out.push(`INSERT INTO notebook_word (id, notebook_id, dir_id, word_id, sort_order) SELECT '${linkId(l.linkKey)}', n.id, '${l.dirId}', w.id, ${l.sort} FROM word w, notebook n WHERE n.user_id = '${UID}' AND n.name = '梁爽日语课' AND w.user_id = '${UID}' AND w.surface = '${esc(l.surface)}' AND w.reading = '${esc(l.reading)}' ON CONFLICT (dir_id, word_id) DO NOTHING;`);
}
for (const s of sents) {
  out.push(`INSERT INTO notebook_sentence (id, notebook_word_id, text, translation, src, sort_order) VALUES ('nbs-' || substr(md5('${esc(s.linkKey + '|' + s.text)}'),1,24), '${linkId(s.linkKey)}', '${esc(s.text)}', '${esc(s.translation)}', '${esc(s.src)}', ${s.sort}) ON CONFLICT (notebook_word_id, text) DO NOTHING;`);
}
fs.writeFileSync(process.argv[4], out.join('\n') + '\n');
console.log(`${LEVEL}: 词=${words.size} 挂载=${links.length} 句子=${sents.length} SQL行=${out.length} 隔离跳过: 词=${skippedWords} 句=${skippedSents}`);
