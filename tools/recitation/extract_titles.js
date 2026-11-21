// 从各册讲义 md 头部提取 30 课标题（LLM 去振假名、还原标准写法）
// 用法: node extract_titles.js out/titles.json
const fs = require('fs');
const Q = require('../liangshuang/qa_lib');

const ROOT = 'F:/0-N1日语精讲高级班课程讲义';
const VOL = {
  12: '初级上', 13: '初级上', 14: '初级上', 15: '初级上', 16: '初级上',
  17: '初级下', 18: '初级下', 19: '初级下', 20: '初级下', 21: '初级下',
  23: '初级下', 24: '初级下', 25: '初级下',
  26: '中级上', 27: '中级上', 28: '中级上', 29: '中级上', 30: '中级上', 31: '中级上', 32: '中级上',
  33: '中级下', 34: '中级下', 35: '中级下', 36: '中级下', 37: '中级下', 38: '中级下', 39: '中级下',
  40: '高级', 41: '高级', 42: '高级', 43: '高级', 44: '高级', 45: '高级',
};

const heads = [];
for (const [n, vol] of Object.entries(VOL)) {
  const p = `${ROOT}/${vol}/_summary/第${n}课.md`;
  if (!fs.existsSync(p)) { console.error('缺文件', p); continue; }
  const lines = fs.readFileSync(p, 'utf8').split(/\r?\n/).slice(0, 14).join('\n');
  heads.push(`=== 第${n}课 ===\n${lines}`);
}

const prompt = `下面是日语课程每课讲义的开头片段（含振假名、PDF 转写噪音）。请为每课提取「课名」：
- 通常是「第N課」后面的日文标题（如「オックスフォードへの帰り旅」「和菓子に見る日本の美意識」）；
- 去掉振假名标注、空格和 PDF 断行噪音，还原成规范日文；
- 不要带「第N課」前缀，不要带「总结」「学习目标」等字样；
- 只输出 JSON 数组：[{"lesson":12,"title":"漫画"},...]

${heads.join('\n\n')}`;

(async () => {
  const arr = await Q.llm(prompt, 3);
  if (!Array.isArray(arr)) { console.error('标题提取失败'); process.exit(1); }
  const out = {};
  for (const it of arr) {
    const n = Number(it.lesson);
    if (!Number.isFinite(n) || !it.title) continue;
    out[n] = String(it.title).trim();
  }
  fs.writeFileSync(process.argv[2], JSON.stringify(out, null, 1));
  const missing = Object.keys(VOL).map(Number).filter(n => !out[n]);
  console.log(`标题 ${Object.keys(out).length} 条，缺 ${missing.join(',') || '无'}`);
  for (const n of Object.keys(VOL).map(Number).sort((a, b) => a - b)) console.log(n, out[n]);
})();
