// 生词本导入：chuji-shang-full.json → SQL → psql。
// 用法: node import_notebook.js <json> <userId> <outSql>
const fs = require('fs');
const data = JSON.parse(fs.readFileSync(process.argv[2], 'utf8'));
const UID = process.argv[3];
const NB = 'nb-liangshuang';
const esc = s => String(s ?? '').replace(/'/g, "''");

// 词去重（surface|reading），保留首个非空 pos/meaning
const words = new Map();
const links = [];   // {dirId, surface, reading, sort}
const sents = [];   // {linkKey, text, translation, src}
for (const L of data.lessons) {
  const ln = String(L.lesson).padStart(2, '0');
  const ldir = `nbd-cjs-L${ln}`;
  L.categories.forEach((c, ci) => {
    const cdir = `${ldir}-c${ci}`;
    c.words.forEach((w, wi) => {
      const reading = w.reading || '';
      const key = w.surface + '|' + reading;
      if (!words.has(key)) words.set(key, { surface: w.surface, reading, pos: w.pos || '', meaning: w.meaning || '' });
      else {
        const e = words.get(key);
        if (!e.pos && w.pos) e.pos = w.pos;
        if (!e.meaning && w.meaning) e.meaning = w.meaning;
      }
      const linkKey = `${cdir}::${key}`;
      links.push({ dirId: cdir, surface: w.surface, reading, sort: wi, linkKey });
      (w.sentences || []).forEach((s, si) => sents.push({ linkKey, text: s.text, translation: s.translation || '', src: s.src || '', sort: si }));
    });
  });
}

const out = [];
out.push(`DELETE FROM notebook WHERE id = '${NB}';`); // 幂等重跑（级联清 dir/word/sentence，不动全局 word）
out.push(`INSERT INTO notebook (id, user_id, name) VALUES ('${NB}', '${UID}', '梁爽日语课');`);
out.push(`INSERT INTO notebook_dir (id, notebook_id, parent_id, name, sort_order) VALUES ('nbd-cjs', '${NB}', NULL, '初级上', 0);`);
for (const L of data.lessons) {
  const ln = String(L.lesson).padStart(2, '0');
  out.push(`INSERT INTO notebook_dir (id, notebook_id, parent_id, name, sort_order) VALUES ('nbd-cjs-L${ln}', '${NB}', 'nbd-cjs', '第${L.lesson}课', ${L.lesson});`);
  L.categories.forEach((c, ci) => {
    out.push(`INSERT INTO notebook_dir (id, notebook_id, parent_id, name, sort_order) VALUES ('nbd-cjs-L${ln}-c${ci}', '${NB}', 'nbd-cjs-L${ln}', '${esc(c.name)}', ${ci});`);
  });
}
for (const [key, w] of words) {
  out.push(`INSERT INTO word (id, user_id, surface, reading, pos, meaning) VALUES ('w-' || substr(md5('${esc(key)}'),1,24), '${UID}', '${esc(w.surface)}', '${esc(w.reading)}', '${esc(w.pos)}', '${esc(w.meaning)}') ON CONFLICT (user_id, surface, reading) DO NOTHING;`);
}
// linkKey → 确定性 notebook_word id（md5，避免日文字符被滤空撞 id）
const crypto = require('crypto');
const linkId = k => 'nbw-' + crypto.createHash('md5').update(k).digest('hex').slice(0, 24);
for (const l of links) {
  out.push(`INSERT INTO notebook_word (id, notebook_id, dir_id, word_id, sort_order) SELECT '${linkId(l.linkKey)}', '${NB}', '${l.dirId}', w.id, ${l.sort} FROM word w WHERE w.user_id = '${UID}' AND w.surface = '${esc(l.surface)}' AND w.reading = '${esc(l.reading)}' ON CONFLICT (dir_id, word_id) DO NOTHING;`);
}
for (const s of sents) {
  out.push(`INSERT INTO notebook_sentence (id, notebook_word_id, text, translation, src, sort_order) VALUES ('nbs-' || substr(md5('${esc(s.linkKey + '|' + s.text)}'),1,24), '${linkId(s.linkKey)}', '${esc(s.text)}', '${esc(s.translation)}', '${esc(s.src)}', ${s.sort}) ON CONFLICT (notebook_word_id, text) DO NOTHING;`);
}
fs.writeFileSync(process.argv[4], out.join('\n') + '\n');
console.log(`词=${words.size} 挂载=${links.length} 句子=${sents.length} SQL行=${out.length}`);
