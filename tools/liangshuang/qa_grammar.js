// QA 关③ 语法：LLM 查截断/错字/中日混杂/明显语法错误；能自动修的回写语料，修不出隔离。
// 口语省略/风格不挑。用法: node qa_grammar.js <fullJson>
const Q = require('./qa_lib');

const jp = process.argv[2];
const d = Q.loadCorpus(jp);
const q = Q.loadQ();

const byText = new Map();
Q.eachSentence(d, (L, c, w, s) => {
  if (!byText.has(s.text)) byText.set(s.text, { text: s.text, lesson: L.lesson });
});
const items = [...byText.values()];
console.log('待审句子(去重):', items.length);

let fixed = 0, quar = 0, report = 0;
const byType = {};
(async () => {
  await Q.mapPool(items, 12, async (batch) => {
    const lines = batch.map((x, i) => `${i}. ${x.text}`).join('\n');
    const r = await Q.llm(`你是日语教学专家。逐句检查：截断不完整、错字、中日文混杂、明显语法错误（助词/活用）。
口语体/省略尾/数字与假名混写（教材写法）属正常，不要挑。
只输出有问题项 JSON 数组（不要围栏）：[{"i":0,"type":"truncated|typo|mixed|grammar","fix":"修正后的完整句子"}]
没问题输出 []。
${lines}`);
    for (const f of (r || [])) {
      const x = batch[f.i];
      if (!x) continue;
      byType[f.type] = (byType[f.type] || 0) + 1;
      if (f.fix && Q.isPureJa(f.fix) && Q.saneFix(f.fix, x.text) && f.fix !== x.text && f.type !== 'grammar') {
        Q.eachSentence(d, (L, c, w, s) => { if (s.text === x.text) s.text = f.fix; });
        x.text = f.fix;
        fixed++;
      } else if (f.type === 'grammar') {
        console.log(`  [report-only] grammar: ${x.text} => ${f.fix || f.problem || ''}`);
        report++;
      } else {
        Q.addQ(q, { stage: 'grammar', reason: f.type || 'grammar', text: x.text, lesson: x.lesson });
        quar++;
      }
    }
  });
  Q.saveCorpus(jp, d);
  Q.saveQ(q);
  console.log(`qa_grammar: 回写修正=${fixed} 隔离=${quar} 仅报告=${report} 类型=${JSON.stringify(byType)} 隔离总数=${q.length}`);
})();
