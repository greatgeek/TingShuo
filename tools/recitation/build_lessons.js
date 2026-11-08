// 生成课文本数据：SRT cue → 课文行（可合并/改写）+ 音频时长
// 用法: node build_lessons.js out/srt.json out/lessons.json
const fs = require('fs');

const TITLES = {
  5: '初対面',
  6: 'Wi-Fiはありますか',
  7: 'スマホに写真がありますよ',
  8: 'Sサイズでお願いします',
  9: '誕生日と性格',
};

// cue 文本修正（不合并）：key = cue 序号（1-based）
const FIXES = {
  5: {
    1: 'あ、李さんですか。',
    2: '初めまして、中村絵里です。',
    5: '中村さんですね。はい。李さんは新入生ですか？',
    13: '紹介します。李由加さんです。',
    14: '初めまして、李由加です。',
  },
  6: {
    15: 'ありがとうございました。それから、これ、ほんの気持ちです。',
  },
  7: {
    21: 'ハチ君、かわいい。',
  },
  8: {},
  // 第9课 SRT 是自动识别稿，按教材整句重组（cue 区间 → 课文句）
  9: [
    { cues: [1, 2], text: 'ゆかちゃんのこのスマホケース、かわいいですね。' },
    { cues: [3, 6], text: 'ありがとう。私はかに座で、ピンクは今年のラッキーカラーです。' },
    { cues: [7, 9], text: 'そうですか。日本の留学生活はどうですか。' },
    { cues: [10, 10], text: '順調ですよ。' },
    { cues: [11, 11], text: 'ゆかちゃんは性格が明るいですからね。' },
    { cues: [12, 15], text: 'いえ、中村さんのおかげですよ。いろいろありがとう。' },
    { cues: [16, 16], text: 'どういたしまして。' },
    { cues: [17, 19], text: '中村さんは、もしかして、山羊座ですか。' },
    { cues: [20, 20], text: 'え？どうしてですか。', startPad: 0.7 },
    { cues: [21, 21], text: 'とても真面目ですから。' },
    { cues: [22, 29], text: 'え？真面目ですか？いえいえ、私はそんなに真面目じゃありません。でも、山羊座ですよ。' },
    { cues: [31, 31], text: 'じゃ、お誕生日は来月ですね。', startPad: 0.5 },
    { cues: [32, 32], text: 'はい。1月13日です。' },
    { cues: [33, 35], text: 'ええと、土曜日ですね！誕生日パーティーをしましょう！' },
    { cues: [36, 39], text: 'え？本当ですか。嬉しいです。ありがとう。' },
  ],
};

function norm(s) {
  return s
    .replace(/\s+/g, ' ')
    .replace(/\s+(?=[。、？！])/g, '')
    .replace(/([。、？！])\s+/g, '$1')
    .replace(/\?/g, '？')
    .replace(/!/g, '！')
    .trim();
}

const src = JSON.parse(fs.readFileSync(process.argv[2], 'utf8'));
const out = [];
for (const L of src) {
  const cues = L.cues;
  const lines = [];
  if (Array.isArray(FIXES[L.lesson])) {
    for (const spec of FIXES[L.lesson]) {
      const [a, b] = spec.cues;
      const start = Math.max(0, cues[a - 1].start - (spec.startPad || 0));
      lines.push({ start: Math.round(start * 1000) / 1000, end: cues[b - 1].end, text: spec.text, translation: '' });
    }
  } else {
    const fixes = FIXES[L.lesson] || {};
    cues.forEach((c, i) => {
      lines.push({ start: c.start, end: c.end, text: norm(fixes[i + 1] || c.text), translation: '' });
    });
  }
  // 首尾相接：后一行起点不得早于前一行终点
  for (let i = 1; i < lines.length; i++) {
    if (lines[i].start < lines[i - 1].end) lines[i].start = lines[i - 1].end;
  }
  out.push({
    lesson: L.lesson,
    title: `第${L.lesson}课 ${TITLES[L.lesson]}`,
    audioKey: `recitation-lesson-${String(L.lesson).padStart(2, '0')}`,
    lines,
  });
}
fs.writeFileSync(process.argv[3], JSON.stringify({ lessons: out }, null, 1));
for (const L of out) {
  console.log(`第${L.lesson}课 ${L.lines.length} 行`);
  L.lines.forEach((l, i) => console.log(`  ${i + 1}\t${l.start}-${l.end}\t${l.text}`));
}
