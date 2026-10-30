// QA 关⑤ 配音前置：词读音校验（读音决定单词 TTS 发音）+ 句子合成预检（中文/超长禁入隔离）。
// 读音修正回写语料并产出 word 表同步 SQL。用法: node qa_audio.js <fullJson>
const fs = require('fs');
const Q = require('./qa_lib');

const jp = process.argv[2];
const d = Q.loadCorpus(jp);
const q = Q.loadQ();

// 词读音校验：仅审含汉字且有读音的词
const words = new Map();
for (const L of d.lessons) for (const c of L.categories) for (const w of c.words) {
  const k = w.surface + '|' + (w.reading || '');
  if (/[一-鿯]/.test(w.surface) && w.reading && Q.KANA_ONLY.test(w.reading) && !words.has(k)) {
    words.set(k, { surface: w.surface, reading: w.reading, meaning: w.meaning || '' });
  }
}
const items = [...words.values()];
console.log('待审词读音:', items.length);

let fixed = 0, quar = 0;
const sqlLines = [];
(async () => {
  await Q.mapPool(items, 50, async (batch) => {
    const lines = batch.map((x, i) => `${i}. ${x.surface}｜${x.reading}｜${x.meaning}`).join('\n');
    const r = await Q.llm(`你是日语教学专家。判断每个词的假名读音是否正确（含声调无关的读音层面；长短音/促音错误也算）。
只输出错误项 JSON 数组（不要围栏）：[{"i":0,"fix":"正确平假名读音"}]
全对输出 []。
${lines}`);
    for (const f of (r || [])) {
      const x = batch[f.i];
      if (!x || !f.fix) continue;
      if (/^[ぁ-ゖー]+$/.test(f.fix) && f.fix !== x.reading) {
        for (const L of d.lessons) for (const c of L.categories) for (const w of c.words) {
          if (w.surface === x.surface && w.reading === x.reading) w.reading = f.fix;
        }
        sqlLines.push(`UPDATE word SET reading = '${f.fix.replace(/'/g, "''")}' WHERE surface = '${x.surface.replace(/'/g, "''")}' AND reading = '${x.reading.replace(/'/g, "''")}';`);
        fixed++;
      } else {
        Q.addQ(q, { stage: 'audio', reason: 'reading', surface: x.surface, reading: x.reading });
        quar++;
      }
    }
  });

  // 句子合成预检：不纯净/超长的句禁入合成（服务端 /audio/say 同规则护栏）
  let blocked = 0;
  const seen = new Set();
  Q.eachSentence(d, (L, c, w, s) => {
    if (seen.has(s.text)) return;
    seen.add(s.text);
    if (!Q.isPureJa(s.text)) {
      Q.addQ(q, { stage: 'audio', reason: 'audio-block', text: s.text, lesson: L.lesson });
      blocked++;
    }
  });

  Q.saveCorpus(jp, d);
  Q.saveQ(q);
  if (sqlLines.length) fs.writeFileSync(process.env.TEMP + '/word-reading-fix.sql', sqlLines.join('\n') + '\n');
  console.log(`qa_audio: 读音回写=${fixed} 读音隔离=${quar} 合成禁入=${blocked} SQL=${sqlLines.length} 隔离总数=${q.length}`);
})();
