// 课文本长文数据的定点修订：错字修正、说话人前缀剥离、第40课上下段分界
// 用法: node patch_longtext.js
const fs = require('fs');

const file = 'out/longtext-lines.json';
const data = JSON.parse(fs.readFileSync(file, 'utf8'));

const FIXES = {
  27: [
    ['好不容易', 'せっかく'],
    ['聞きけた', '聞けた'],
  ],
  29: [
    ['今_HTTPseason', '今シーズン'],
    ['2月12日', '2月22日'],
  ],
  40: [
    ['社内内で', '社内で'],
  ],
};
for (const [n, list] of Object.entries(FIXES)) {
  if (!data[n]) continue;
  for (const l of data[n]) {
    for (const [a, b] of list) l.text = l.text.split(a).join(b);
  }
}

if (data[40]) {
  const NAME = /^(石田|由加|鈴木|梅ノ屋の社員|社員)[：:]\s*/;
  let part = 1;
  for (const l of data[40]) {
    if (/^石田[：:]由加さん/.test(l.text) || /^由加[：:]はい/.test(l.text)) part = 2;
    l.text = l.text.replace(NAME, '');
    if (l.translation) l.translation = l.translation.replace(NAME, '');
    if (part === 2) l.part = 2;
  }
  const total = data[40].length;
  const p2 = data[40].filter(l => l.part === 2).length;
  console.log(`第40课: 总=${total} 段2=${p2} 段1=${total - p2}`);
}

for (const n of Object.keys(FIXES)) {
  const zhBreak = data[n] ? data[n].filter(l => /[ぁ-んァ-ヶ]/.test(l.text) && /[们这说时对过还个么样发觉为给没两听写读语书门问题间开关长东车马鸟龙让认讲练习汉]/.test(l.text)) : [];
  if (zhBreak.length) {
    console.log(`第${n}课 可疑中文混入:`);
    zhBreak.forEach(l => console.log('  ' + l.text));
  }
}

fs.writeFileSync(file, JSON.stringify(data, null, 1));
console.log('已写回', file);
