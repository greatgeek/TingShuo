// 梁爽讲义解析器（初级上/初级下）：从 _summary/第N课.md 抽取
//   三个词表（会话用单词或课文用单词/语法解释用单词/习题用单词）+ 句子池（会话原文/语法例句/听力训练）
//   并做「汉字+振り仮名交错」清洗与 词↔句 绑定。
//   初级下第17/24/25课无「## 会话原文」，改用「课文」「会 话」等裸标题，需走回退匹配。
// 用法: node parse_chujishang.js <summaryDir> <outJson>
const fs = require('fs');
const path = require('path');

const KANA = '^[ぁ-んァ-ヶー]+$';
const reKana = new RegExp(KANA);
const KANJI = '[㐀-䶿一-鿿]';
const reKanji = new RegExp(KANJI);
// 助词/送假名/补助动词：纯假名但属于正文，不能当振り仮名丢掉
const KEEP = new Set(['は','が','を','に','で','と','も','へ','か','よ','ね','さ','て','わ','な','の','や','ぞ','ぜ','ば','っ',
  'です','ます','でした','ました','でしょ','でしょう','ください','ましょう','ながら','ても','しか','まで','だけ','ほど','くらい','ぐらい','など',
  'た','て','る','れ','ら','ない','ある','いる','する','くる','きます','きました','します','しました','たい','だ','じゃ','って','ても','から','ので']);

// 清洗交错振り仮名 → { text, ruby:[[kanji,kana],...] }
function cleanJP(raw) {
  // 先把（）组前后补空格，避免「由（ゆ）」粘在一个 token 里；兼容半角括号
  raw = raw.replace(/([(（][^)）]*[)）])/g, ' $1 ');
  const tokens = raw.match(/[(（][^)）]*[)）]|\S+/g) || [];
  const out = [];
  const ruby = [];
  let prevKanji = null;
  for (let i = 0; i < tokens.length; i++) {
    const tok = tokens[i];
    if (tok.startsWith('（') || tok.startsWith('(')) {
      const inner = tok.slice(1, -1);
      if (inner.trim() === '') { prevKanji = null; continue; }            // 填空（ ）
      if (reKanji.test(inner)) { prevKanji = null; continue; }            // 中文舞台提示，丢弃
      const m = inner.match(/^([ぁ-んァ-ヶー]+)([\s\S]*)$/);
      if (m) {
        if (prevKanji) ruby.push([prevKanji, m[1]]);
        if (m[2].trim()) out.push(m[2].trim());
      }
      prevKanji = null;
      continue;
    }
    if (reKana.test(tok)) {
      const next = tokens[i + 1] || '';
      const nextIsKanji = reKanji.test(next.replace(/[(（][^)）]*[)）]/g, ''));
      if (prevKanji && !KEEP.has(tok)) { ruby.push([prevKanji, tok]); prevKanji = null; continue; } // 后置空格振り仮名
      if (!prevKanji && nextIsKanji && !KEEP.has(tok) && tok.length >= 2) { prevKanji = null; continue; } // 前置振り仮名（中 なか むら 村）
      out.push(tok); prevKanji = null; continue;                                                  // 助词/独立假名词
    }
    out.push(tok);
    const m = tok.match(new RegExp('(' + KANJI + '+)$'));
    prevKanji = m ? m[1] : null;
  }
  return { text: out.join(''), ruby };
}

// 词表条目解析（会话/语法为 **N.** **词** 格式；习题为 "N 词" 格式）
function parseWords(block) {
  const words = [];
  const lines = block.split('\n');
  for (const raw of lines) {
    const line = raw.replace(/\r/g, ''); // CRLF：JS 的 . 不匹配 \r，会弄坏 (.+)$
    let m = line.match(/^\*\*\d+\.\*\*\s+(.+)$/) || line.match(/^(\d+)\s+(.+)$/);
    if (!m) continue;
    let body = (m[2] || m[1]).trim();
    if (line.match(/^\*\*\d+\.\*\*/)) body = line.replace(/^\*\*\d+\.\*\*\s*/, '').trim();
    if (/^-?\s*子词条/.test(body)) continue;
    body = body.replace(/\*\*/g, '').trim();
    // 取词面：到 （ 或 【 或 音调符号 或 · 或 ［ 为止
    let mm = body.match(/^(\*\*)?([^（【·［⓪①③④⑤⑦⑧⑨⑪⑫⑬⑭⑮]+?)（/);
    let surface, rest;
    if (mm) { surface = mm[2].replace(/\*\*/g, '').trim(); rest = body.slice(mm[0].length - 1); }
    else {
      mm = body.match(/^(\*\*)?([^·［⓪①③④⑤⑦⑧⑨⑪⑫⑬⑭⑮]+?)([⓪①②③④⑤⑥⑦⑧⑨⑩⑪⑫⑬⑭⑮]+)?\s*(·|［)/);
      if (!mm) {
        // 假名【汉字】型（にぎやか【賑やか】② [形Ⅱ]…）：无括号读音且用半角 [ 时上面两条都不匹配
        mm = body.match(/^(\*\*)?([^（【·［\[]+?)【[^】]*】/);
        if (mm) {
          surface = mm[2].replace(/\*\*/g, '').trim();
          rest = body.slice(mm[0].length);
        } else {
          // 纯假名/无读音型（ガラケー⓪ [名]…、どうも① [副]…）：词性用半角 [ 或 〔〕，无 ·/［
          mm = body.match(/^(\*\*)?([^（【·［\[〔⓪①-⑳]+?)(?=[⓪①-⑳]|\s*(?:·|［|\[|〔)|$)/);
          if (!mm) continue;
          surface = mm[2].replace(/\*\*/g, '').trim();
          rest = body.slice(mm[0].length);
        }
      } else {
        surface = mm[2].replace(/\*\*/g, '').trim();
        rest = body.slice(mm[0].length);
      }
    }
    surface = surface.replace(/\*\*/g, '').trim();
    surface = surface.replace(/【[^】]*】/g, '').replace(/^[-−–]+|[-−–]+$/g, '').trim(); // 去【别名】与接尾辞标记 -
    if (!surface) continue;
    // 读音：第一个（...）内去掉音调
    let reading = '';
    const rm = rest.match(/^（([^）]*)）/);
    if (rm) { reading = rm[1].replace(/[⓪①③④⑤⑦⑧⑨⑪⑫⑬⑭⑮＋+]/g, '').replace(/\s+/g, ''); rest = rest.slice(rm[0].length); }
    // （Asia）（【法】enquête）等是语源/注释而非读音；非假名读音会让词音与导出变脏
    if (reading && !/^[ぁ-ゖァ-ヺー・]+$/.test(reading)) reading = '';
    // 音调
    const pm = (body.match(/([⓪①③④⑤⑦⑧⑨⑪⑫⑬⑭⑮＋+]+)/) || [])[1] || '';
    // 词性：第一个［...］
    const posm = rest.match(/［([^］]*)］/);
    const pos = posm ? posm[1] : '';
    // 级别：尾部 [N5] / [N4,N4] / 裸 N4
    const lvl = (body.match(/\[?(N\d)(?:[,，]N\d)*\]?$/) || [])[1] || '';
    // 中文释义：词性之后到尾部级别之前
    let meaning = rest;
    if (posm) meaning = rest.slice(rest.indexOf('］') + 1);
    meaning = meaning.replace(/·?\s*\[?N\d(?:[,，]N\d)*\]?$/, '').replace(/^[·\s]+/, '').trim();
    if (!surface || !meaning) continue;
    words.push({ surface, reading, pitch: pm, pos, meaning, level: lvl });
  }
  return words;
}

function section(md, startRe, endRe) {
  const s = md.search(startRe);
  if (s < 0) return '';
  const rest = md.slice(s);
  const e = endRe ? rest.slice(1).search(endRe) : -1;
  return e < 0 ? rest : rest.slice(0, e + 1);
}

// 会话/课文正文：初级「## 会话原文」或「会 话/课 文」；中级/高级为裸标题「会話/本文」，振り仮名可能同行
function conversationSection(md) {
  let s = md.search(/^##\s*会话原文\s*$/m);
  let kind = '会话';
  if (s < 0) s = md.search(/^[ \t]*(?:会\s*话|课\s*文|会\s*話|本\s*文)\s*(?:[（(][^）)\n]*[）)])?[ \t]*$/m);
  if (s >= 0 && /本\s*文/.test(md.slice(s).split('\n')[0])) kind = '本文';
  if (s < 0) {
    s = md.search(/^[^\n]{0,16}?会\s*[話话][^\n]{0,8}$/m);
    if (s < 0) { s = md.search(/^[^\n]{0,16}?本\s*文[^\n]{0,8}$/m); kind = '本文'; }
  }
  if (s < 0) return { block: '', kind };
  const rest = md.slice(s);
  const e = rest.slice(1).search(/^\s*(?:###\s*会话用单词|###\s*语法解释用单词|课文(?:用)?单词|.*新出単語)\s*$/m);
  return { block: e < 0 ? rest : rest.slice(0, e + 1), kind };
}

// 主词表：初级「会话用单词 / 课文用单词」；中级/高级「新出単語」
function mainWordSection(md, convKind) {
  let m = md.match(/^###\s*会话用单词\s*$/m);
  let name = '会话用单词';
  if (!m) { m = md.match(/^课文(?:用)?单词\s*$/m); name = '课文用单词'; }
  if (!m) { m = md.match(/^.*新出単語\s*$/m); name = convKind === '本文' ? '课文用单词' : '会话用单词'; }
  if (!m) return { name, block: '' };
  const rest = md.slice(m.index);
  const e = rest.slice(1).search(/^(?:###\s*语法解释用单词|.*解説用単語)\s*$/m);
  return { name, block: e < 0 ? rest : rest.slice(0, e + 1) };
}

function parseLesson(file) {
  const md = fs.readFileSync(file, 'utf8');
  const lesson = (path.basename(file).match(/第(\d+)课/) || [])[1];
  const convSec = conversationSection(md);
  const conv = convSec.block;
  const wMain = mainWordSection(md, convSec.kind);
  const wConv = wMain.block;
  const wGram = section(md, /(?:###\s*语法解释用单词|^.*解説用単語\s*$)/m, /(?:##\s*语法解释(?!用)|^.*文法解説\s*$)/m);
  const gram = section(md, /(?:##\s*语法解释(?!用)|^.*文法解説\s*$)/m, /(?:习题用单词|^.*練習用単語\s*$)/m);
  const wExer = section(md, /(?:习题用单词|^.*練習用単語\s*$)/m, /(?:听力训练|^.*練習問題\s*$)/m);
  const listen = section(md, /听力训练/, /$/);

  // 句子池
  const sentences = [];
  const pushSent = (raw, src) => {
    let jp = raw, zh = '';
    const slash = jp.lastIndexOf('／');
    if (slash > 0) { zh = jp.slice(slash + 1).trim(); jp = jp.slice(0, slash); }
    jp = jp.replace(/^[①-⑩例（）\d０-９\s]+/, '');
    jp = jp.replace(/^([A-ZＡ-Ｚ]:|[^：:]{1,12}：)/, ''); // 说话人前缀（A: / 由加：）
    if (!jp) return;
    const c = cleanJP(jp);
    const kanaCount = (c.text.match(/[ぁ-んァ-ヶ]/g) || []).length;
    if (c.text.length < 4 || kanaCount < 2) return; // 丢纯中文/过短残句
    // 交错排版的 ruby 配对不可靠，导入句只存干净原文+译文，不存 ruby（避免错配误导）
    sentences.push({ text: c.text, ruby: [], translation: zh, src });
  };
  for (const line of conv.split('\n')) {
    const t = line.replace(/\r/g, '').trim();
    if (!t) continue;
    pushSent(t, 'conversation');
  }
  for (const line of gram.split('\n')) {
    const t = line.replace(/\r/g, '').trim();
    const m = t.match(/^[①-⑩]\s*(.+)$/);
    if (m && m[1]) pushSent(m[1], 'grammar');
  }
  for (const line of listen.split('\n')) {
    const t = line.replace(/\r/g, '').trim();
    const m = t.match(/^例（れい）：(.+)$/);
    if (m) pushSent(m[1], 'listening');
  }

  const categories = [
    { name: wMain.name, words: parseWords(wConv) },
    { name: '语法解释用单词', words: parseWords(wGram) },
    { name: '习题用单词', words: parseWords(wExer) },
  ];
  // 词↔句绑定
  for (const cat of categories) {
    for (const w of cat.words) {
      w.sentences = sentences.filter(s =>
        (w.surface && s.text.includes(w.surface)) ||
        (w.reading && w.reading.length > 1 && s.text.includes(w.reading))
      ).map(s => ({ text: s.text, ruby: s.ruby, translation: s.translation }));
    }
  }
  return {
    lesson: Number(lesson),
    categories,
    sentencePool: sentences.length,
    unbound: categories.reduce((n, c) => n + c.words.filter(w => !w.sentences.length).length, 0),
    // 原文留档给 clean_sentences.js：PDF 转写粘连严重时由 LLM 按原文还原，正则清洗只当兜底
    raw: { mainName: wMain.name, conversation: conv, grammar: gram },
  };
}

const dir = process.argv[2];
const out = process.argv[3];
const vol = path.basename(path.dirname(path.resolve(dir)));
const level1 = /^(?:初级[上下]|中级[上下]|高级)$/.test(vol)
  ? vol
  : (dir.match(/(?:初级|中级)[上下]|高级/) || ['初级上'])[0];
const files = fs.readdirSync(dir).filter(f => /^第\d+课\.md$/.test(f))
  .sort((a, b) => Number(a.match(/\d+/)[0]) - Number(b.match(/\d+/)[0]));
const lessons = files.map(f => parseLesson(path.join(dir, f)));
fs.writeFileSync(out, JSON.stringify({ notebook: '梁爽日语课', level1, lessons }, null, 1));

// 统计
let tw = 0, uniq = new Set(), unbound = 0, sents = 0;
for (const L of lessons) {
  sents += L.sentencePool;
  for (const c of L.categories) for (const w of c.words) {
    tw++; uniq.add(w.surface + '|' + w.reading);
    if (!w.sentences.length) unbound++;
  }
}
console.log(`课数=${lessons.length} 词条=${tw} 去重词=${uniq.size} 句子池=${sents} 无句词=${unbound}`);
for (const L of lessons) {
  console.log(`  第${L.lesson}课: 会话${L.categories[0].words.length} 语法${L.categories[1].words.length} 习题${L.categories[2].words.length} 句池${L.sentencePool} 无句${L.unbound}`);
}
