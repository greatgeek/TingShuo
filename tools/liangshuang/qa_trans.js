// QA 关② 译文：LLM 判完整性/意思匹配/中文；不合格自动回写修正译文，修不出则隔离。
// 译文为空直接隔离。用法: node qa_trans.js <fullJson>
const Q = require('./qa_lib');

const jp = process.argv[2];
const d = Q.loadCorpus(jp);
const q = Q.loadQ();

const byText = new Map();
Q.eachSentence(d, (L, c, w, s) => {
  if (!byText.has(s.text)) byText.set(s.text, { text: s.text, trans: s.translation || '', lesson: L.lesson });
});
const empty = [...byText.values()].filter(x => !x.trans);
for (const x of empty) {
  Q.addQ(q, { stage: 'trans', reason: 'trans-empty', text: x.text, lesson: x.lesson });
  byText.delete(x.text);
}
const items = [...byText.values()];
console.log('待审译文(去重):', items.length, '空译文隔离:', empty.length);

let fixed = 0, bad = 0;
(async () => {
  await Q.mapPool(items, 20, async (batch) => {
    const lines = batch.map((x, i) => `${i}. 日：${x.text}\n   中：${x.trans}`).join('\n');
    const r = await Q.llm(`你是日语教学专家。逐条判断中文译文：是否完整、是否与日文意思匹配、是否是自然中文。
有问题的给出修正译文；完全正确的不要输出。
只输出 JSON 数组（不要围栏）：[{"i":0,"fix":"修正后的中文译文"}]
${lines}`);
    for (const f of (r || [])) {
      const x = batch[f.i];
      if (!x || !f.fix) continue;
      if (Q.isZh(f.fix) && Q.saneTrans(f.fix)) {
        Q.eachSentence(d, (L, c, w, s) => { if (s.text === x.text) s.translation = f.fix; });
        fixed++;
      } else {
        Q.addQ(q, { stage: 'trans', reason: 'trans', text: x.text, lesson: x.lesson });
        bad++;
      }
    }
  });
  Q.saveCorpus(jp, d);
  Q.saveQ(q);
  console.log(`qa_trans: 回写修正=${fixed} 隔离=${bad} 隔离总数=${q.length}`);
})();
