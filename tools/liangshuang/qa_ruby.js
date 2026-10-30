// QA 关④ 注音：结构审计（对齐/缺读音/非法字符）+ LLM 逐对核对汉字读音；
// 误读写入 ruby_override.json（rubyfill/服务端生成时优先于 kagome）。
// 用法: node qa_ruby.js [psqlBin]（默认本地库）
const { execFileSync } = require('child_process');
const Q = require('./qa_lib');

const PSQL = process.argv[2] || 'G:/TingShuo/pgsql/bin/psql';
const out = execFileSync(PSQL, ['-U', 'tingshuo', '-d', 'tingshuo', '-t', '-A', '-F', '\x01',
  '-c', 'SELECT text, ruby_tokens FROM notebook_sentence ORDER BY id'], { maxBuffer: 1 << 28 }).toString('utf8');
const rows = out.split('\n').filter(x => x.trim()).map(l => {
  const [text, ruby] = l.split('\x01');
  return { text, toks: JSON.parse(ruby || '[]') };
});
const q = Q.loadQ();
const KANJI = /[一-鿯]/;
const HIRA = /^[ぁ-ゖー]+$/;

let misalign = 0, noRead = 0, badChars = 0;
for (const r of rows) {
  if (r.toks.map(t => t[0]).join('') !== r.text) { Q.addQ(q, { stage: 'ruby', reason: 'ruby-struct', text: r.text }); misalign++; continue; }
  for (const [w, rd] of r.toks) {
    if (KANJI.test(w)) {
      if (!rd) { Q.addQ(q, { stage: 'ruby', reason: 'ruby-struct', text: r.text }); noRead++; }
      else if (!HIRA.test(rd)) { Q.addQ(q, { stage: 'ruby', reason: 'ruby-struct', text: r.text }); badChars++; }
    }
  }
}
console.log(`结构审计: misalign=${misalign} noReading=${noRead} badChars=${badChars}`);

const ov = Q.loadOverride();
const votes = {};
let flags = 0;
(async () => {
  await Q.mapPool(rows, 12, async (batch) => {
    const lines = batch.map((r, i) => {
      const pairs = r.toks.filter(t => t[1]).map(t => `${t[0]}→${t[1]}`).join(' ');
      return `${i}. ${r.text}　[注音: ${pairs || '（無）'}]`;
    }).join('\n');
    const res = await Q.llm(`你是日语教学专家。核对每句所给汉字注音是否正确（含语境读音，如「この後」的「後」=あと、「あの方」的「方」=かた）。
只输出错误项 JSON 数组（不要围栏）：[{"i":0,"w":"汉字词","right":"正确平假名"}]
全对输出 []。
${lines}`);
    for (const f of (res || [])) {
      const r = batch[f.i];
      if (!r || !f.w || !f.right) continue;
      const cur = (r.toks.find(t => t[0] === f.w) || [])[1];
      if (cur && cur !== f.right && HIRA.test(f.right)) {
        flags++;
        votes[f.w + '→' + f.right] = (votes[f.w + '→' + f.right] || 0) + 1;
        ov[f.w] = f.right;
      }
    }
  });
  Q.saveOverride(ov);
  Q.saveQ(q);
  const top = Object.entries(votes).sort((a, b) => b[1] - a[1]).slice(0, 20);
  console.log(`qa_ruby: LLM 误读标记=${flags} 覆盖表条目=${Object.keys(ov).length}`);
  console.log('高频候选(需人工过目):', top.map(([k, v]) => `${k}×${v}`).join(' '));
})();
