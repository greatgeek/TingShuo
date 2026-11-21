// 无教材正文的课（高级43-45）：按音频 ASR 稿重建课文行（合并碎片、修同音错字、补译文）
// 用法: node clean_srt_lessons.js out/srt-all.json out/srt-lines.json 43,44,45
const fs = require('fs');
const Q = require('../liangshuang/qa_lib');

const srt = JSON.parse(fs.readFileSync(process.argv[2], 'utf8'));
const outFile = process.argv[3];
const wanted = (process.argv[4] || '43,44,45').split(',').map(Number);

function prompt(lessonNo, cues) {
  const listing = cues.map((c, i) => `${i + 1}. ${c.text}`).join('\n');
  return `下面是日语课文朗读视频的自动语音识别稿，第${lessonNo}课。识别稿按字幕切成了碎片，常有同音错字、断句碎片、多字少字（例如「技本」应为「技術」、「カシノ」应为「和菓子の」）。请把碎片整理成完整、通顺的日语句子：

要求：
- 只做还原与明显的识别纠错，不要改写内容、不要增删句子；
- 按语义合并相邻碎片成一句，句末用「。」「？」「！」；
- 每条给出它覆盖的编号区间 [a, b]（从 1 开始，必须连续且互不重叠、升序）；
- 跳过片头标题「第N。」和片尾致谢等与课文无关的内容；
- 同时给出自然、完整的中文译文；
- 只输出 JSON 数组，不要围栏：[{"a":1,"b":3,"text":"日文","translation":"中文"}]

识别稿：
${listing}`;
}

function saneText(t) {
  return typeof t === 'string' && t.trim().length >= 2 && /[ぁ-んァ-ヶ]/.test(t);
}
function saneZh(t) {
  return typeof t === 'string' && t.trim().length >= 1 && !/[ぁ-んァ-ヶ]/.test(t);
}

(async () => {
  const out = {};
  for (const L of srt) {
    if (!wanted.includes(L.lesson) || L.part) continue;
    const cues = L.cues;
    let arr = await Q.llm(prompt(L.lesson, cues), 4);
    if (!Array.isArray(arr)) { console.error(`第${L.lesson}课 重建失败`); continue; }
    // 区间合法化
    let cursor = 1;
    const lines = [];
    for (const it of arr) {
      let a = Number(it.a), b = Number(it.b);
      if (!(a >= 1 && b >= 1 && a <= b && b <= cues.length)) continue;
      if (a < cursor) a = cursor;
      if (b < a) continue;
      if (!saneText(it.text)) continue;
      lines.push({
        a, b,
        start: Math.max(0, Math.round((cues[a - 1].start - 0.06) * 1000) / 1000),
        end: Math.round((cues[b - 1].end + 0.08) * 1000) / 1000,
        text: it.text.trim(),
        translation: saneZh(it.translation) ? it.translation.trim() : '',
      });
      cursor = b + 1;
    }
    const covered = lines.reduce((n, l) => n + l.b - l.a + 1, 0);
    console.log(`第${L.lesson}课: 行=${lines.length} 覆盖cue=${covered}/${cues.length}`);
    lines.forEach((l, i) => console.log(`  ${i + 1}\t${l.start}-${l.end}\t${l.text} | ${l.translation}`));
    out[L.lesson] = { lines };
    await new Promise(r => setTimeout(r, 400));
  }
  fs.writeFileSync(outFile, JSON.stringify(out, null, 1));
})();
