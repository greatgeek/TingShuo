// QA 关① 解析自检：句-词绑定、读音格式、重复句、表达扩展溯源、每课统计。
// 用法: node qa_parse.js <summaryDir> <fullJson>
const fs = require('fs');
const Q = require('./qa_lib');

const [summaryDir, jp] = process.argv.slice(2);
const d = Q.loadCorpus(jp);
const q = Q.loadQ();

let bind = 0, reading = 0, dup = 0, hallucinate = 0;
const stats = {};
const bindCand = [];
for (const L of d.lessons) {
  for (const c of L.categories) {
    const key = `L${L.lesson}/${c.name}`;
    let ws = 0, ss = 0;
    for (const w of c.words) {
      ws++;
      if (w.reading && /[一-鿯【】]/.test(w.reading)) { // 含汉字/语源括注：报告不隔离（词源 gloss 属故意记法）
        console.log('  [report-only] reading:', w.surface, '/', w.reading);
        reading++;
      }
      const seen = new Set();
      for (const s of w.sentences) {
        ss++;
        if (seen.has(s.text)) {
          Q.addQ(q, { stage: 'parse', reason: 'dup', surface: w.surface, text: s.text, lesson: L.lesson });
          dup++;
          continue;
        }
        seen.add(s.text);
        // 含 surface/reading 直接过；活用形等不含的送 LLM 复核，避免误隔离
        if (!s.text.includes(w.surface) && !(w.reading && s.text.includes(w.reading))) {
          bindCand.push({ surface: w.surface, reading: w.reading || '', text: s.text, lesson: L.lesson });
        }
      }
    }
    stats[key] = `${ws}词/${ss}句`;
    if (!ws) console.log('WARN 空类目:', key);
  }
  // 表达扩展溯源：surface 必须在该课源 md 里出现（防 LLM 幻觉造词）
  if (summaryDir) {
    const mdPath = `${summaryDir}/第${L.lesson}课.md`;
    const md = (fs.existsSync(mdPath) ? fs.readFileSync(mdPath, 'utf8') : '').replace(/[（(][^）)]*[）)]/g, '');
    for (const c of L.categories) {
      if (!/表达扩展/.test(c.name)) continue;
      for (const w of c.words) {
        if (md && !md.includes(w.surface)) {
          console.log('  [report-only] hallucinate?:', L.lesson, w.surface);
          hallucinate++;
        }
      }
    }
  }
}
Q.saveQ(q);
console.log('每课统计:', JSON.stringify(stats, null, 0).slice(0, 1200));
console.log(`qa_parse 初筛: bind候选=${bindCand.length} reading=${reading} dup=${dup} hallucinate=${hallucinate}`);

// bind 候选送 LLM 复核（活用形/近义词例句属正常），确认无关才隔离
(async () => {
  if (bindCand.length) {
    await Q.mapPool(bindCand, 20, async (batch) => {
      const lines = batch.map((x, i) => `${i}. 词=${x.surface}（${x.reading}） 句=${x.text}`).join('\n');
      const r = await Q.llm(`判断每句是否可作为该词的例句（含活用形/派生形/惯用搭配，算相关）。
只输出不相关的 JSON 数组（不要围栏）：[{"i":0}]
全部相关输出 []。
${lines}`);
      for (const f of (r || [])) {
        const x = batch[f.i];
        if (!x) continue;
        Q.addQ(q, { stage: 'parse', reason: 'bind', surface: x.surface, text: x.text, lesson: x.lesson });
        bind++;
      }
    });
    Q.saveQ(q);
  }
  console.log(`qa_parse 终: bind=${bind} 隔离总数=${q.length}`);
})();
