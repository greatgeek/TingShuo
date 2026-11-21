// 汇总各课教材会话句（来自讲义清洗产物 out/clean/L*.json）→ sources.json
// 用法: node collect_sources.js
const fs = require('fs');
const path = require('path');
const dir = 'E:/听说app/TingShuo/tools/liangshuang/out/clean';
const out = {};
for (const f of fs.readdirSync(dir).filter(f => /^L\d+\.json$/.test(f))) {
  const n = Number(f.match(/\d+/)[0]);
  const arr = JSON.parse(fs.readFileSync(path.join(dir, f), 'utf8'));
  const lines = arr.filter(x => x.src === 'conversation' && x.text && !/^[（(][^）)]*[）)]$/.test(x.text))
    .map(x => ({ text: x.text, translation: x.translation || '' }));
  if (lines.length) out[n] = { title: `第${n}课`, lines };
}
fs.writeFileSync(path.join(__dirname, 'out', 'sources.json'), JSON.stringify(out, null, 1));
console.log(Object.entries(out).map(([n, v]) => `${n}:${v.lines.length}`).join(' '));
