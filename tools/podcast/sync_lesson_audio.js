#!/usr/bin/env node
// 本课播客同步：扫描 J:/DoubaoDownload/jp_learning（可用 JP_LEARNING_DIR 覆盖），
// 把「生词 第13课.m4a」「语法 第13课.m4a」规范化为 web/media/podcast/word-13.m4a / grammar-13.m4a。
// 音频不进 Git，上线时把 web/media/podcast 上传到 /opt/tingshuo/media/podcast。
const fs = require('fs');
const path = require('path');

const SRC = process.env.JP_LEARNING_DIR || 'J:/DoubaoDownload/jp_learning';
const DST = path.resolve(__dirname, '../../web/media/podcast');
const EXT = ['m4a', 'mp3', 'wav', 'ogg', 'aac'];
const RE = /^(生词|语法)[\s_\-]*第\s*(\d{1,3})\s*[课課]\s*\.(m4a|mp3|wav|ogg|aac)$/i;
const KIND = { 生词: 'word', 语法: 'grammar' };

if (!fs.existsSync(SRC)) {
  console.error(`源目录不存在：${SRC}`);
  process.exit(1);
}
fs.mkdirSync(DST, { recursive: true });

const files = fs.readdirSync(SRC).filter(f => fs.statSync(path.join(SRC, f)).isFile());
const unknown = [];
const seen = new Map();
let copied = 0;
let skipped = 0;

for (const f of files) {
  const m = f.match(RE);
  if (!m) {
    unknown.push(f);
    continue;
  }
  const key = `${KIND[m[1]]}-${parseInt(m[2], 10)}`;
  const ext = m[3].toLowerCase();
  if (seen.has(key)) {
    console.warn(`同名重复：${f} 与 ${seen.get(key)} 都是 ${key}，以后者为准`);
  }
  seen.set(key, f);
  const out = path.join(DST, `${key}.${ext}`);
  for (const e of EXT) {
    const other = path.join(DST, `${key}.${e}`);
    if (other !== out && fs.existsSync(other)) fs.unlinkSync(other);
  }
  const from = path.join(SRC, f);
  const s = fs.statSync(from);
  const stale = !fs.existsSync(out) || fs.statSync(out).size !== s.size || fs.statSync(out).mtimeMs < s.mtimeMs;
  if (stale) {
    fs.copyFileSync(from, out);
    copied++;
    console.log(`同步 ${f} → ${path.relative(process.cwd(), out)}`);
  } else {
    skipped++;
    console.log(`未变 ${f}`);
  }
}

console.log(`\n完成：复制 ${copied}，跳过 ${skipped}；输出目录 ${DST}`);
if (unknown.length) {
  console.log('未识别文件（命名需为「生词 第N课.m4a」或「语法 第N课.m4a」）：');
  unknown.forEach(f => console.log('  ' + f));
}