// 句子还原：初级下 md 由 PDF 转写，振假名括号常把后文一起吞掉
// （考（こうになりました。ありがとうございました。）→ 参考になりました…）。
// 正则会产出截断/插入读音的坏句，这里让 LLM 按原文只做还原并补译文，再重新绑定词↔句。
// 用法: node clean_sentences.js <fullJson> [onlyLesson]
const fs = require('fs');
const path = require('path');
const Q = require('./qa_lib');

const fullFile = process.argv[2];
const only = process.argv[3] ? Number(process.argv[3]) : null;
const d = Q.loadCorpus(fullFile);
const cleanDir = path.join(__dirname, 'out', 'clean');
fs.mkdirSync(cleanDir, { recursive: true });

const CONV_PROMPT = raw => `下面是一课日语教材的「会话/课文」原始转写（来自 PDF）。振假名与正文交错，PDF 转换常把右括号错放到句尾，使括号里混进正文。例如：
考（こうになりました。ありがとうございました。）→ 参考になりました。ありがとうございました。
銀（ぎん） 座（ざで）→ 銀座で
止（や） めました → 止めました
请还原干净、完整的日语句子：
1. 去掉所有振假名标注；被括号吞掉的正文按原顺序接回；跨行断开的词拼接完整。
2. 不要改写措辞，不要增删或纠正教材原句，只做还原。
3. 按说话人的每次发言切分；去掉说话人前缀（如「田中：」）；不要把两个人的问答合并成一条。
4. 跳过无实质内容的行（页码、选项符号、纯中文、舞台说明）。若是问卷，只保留可学习的完整问句/回答句，忽略 a/b 选项标签。
5. 每条给自然、完整的中文翻译。
严格输出 JSON 数组（不要围栏）：[{"text":"干净日文","translation":"中文"}]

原文：
${raw}`;

const GRAM_PROMPT = raw => `下面是日语教材「语法解释」部分的原文（振假名与正文交错，括号可能吞掉后文）。只提取用 ①、②… 标记的日语例句（每个语法点各自编号，全部提取，不要只取前10条），含被换行截断的续行，拼成完整句子；忽略讲解文字。
1. 去掉振假名；括号吞掉的正文按原顺序接回；不要改写措辞。
2. 原文例句后已有“／中文译文”的沿用该译文；没有的补自然中文翻译。
3. 跳过不完整的残句。
严格输出 JSON 数组（不要围栏）：[{"text":"干净日文","translation":"中文"}]

原文：
${raw}`;

// 还原后逐条对照原文，专抓漏词（主语/助词）、残留假名、说话人前缀、译文错位
const VERIFY_PROMPT = (raw, items, kind) => `下面是一课日语教材「${kind}」的原始转写，以及初步还原出的句子 JSON。
请逐条对照原文核对，只修正有问题的条目：
- 漏掉的词/从句（尤其主语、助词、お/ご接头词、被括号吞掉的前半句）；
- 振假名残留或被当成正文重复（如「友達（ともだち）」还原成「友達達」要改回「友達」；「止（や）めました」写成「止やめました」要改回「止めました」）；
- 说话人前缀未去掉、把两个人的问答合并成一条、把 a/b 选项标签混进句子；
- 改写、增删了原句内容；
- 中文译文与日文意思不符——教材自带译文也可能印错（人称、授受方向、主宾颠倒），以日文为准修正；其余保持自然，不要润色。
正确的条目原样保留。严格输出修正后的完整 JSON 数组（不要围栏）：
[{"text":"干净日文","translation":"中文"}]

原始转写：
${raw}

初步结果：
${JSON.stringify(items, null, 1)}`;

function normText(t) {
  return String(t || '')
    .replace(/^[A-Za-zＡ-Ｚ][：:]\s*/, '')
    .replace(/^[^：:]{1,20}[：:]\s*/, '')   // 残留说话人前缀
    .replace(/\s+/g, '')
    .trim();
}

function cleanList(arr, src, log) {
  const out = [];
  const seen = new Set();
  for (const x of (arr || [])) {
    if (!x || !x.text) continue;
    const text = normText(x.text);
    if (text.length < 4 || text.length > 120) continue;
    if (!Q.isPureJa(text)) continue;
    if (seen.has(text)) continue;
    seen.add(text);
    let translation = String(x.translation || '').trim();
    if (translation && !Q.isZh(translation)) translation = '';
    out.push({ text, ruby: [], translation, src });
  }
  log.push(`${src}=${out.length}`);
  return out;
}

(async () => {
  const lessons = d.lessons.filter(L => !only || L.lesson === only);
  const fixesPath = path.join(cleanDir, 'fixes.json');
  const fixes = fs.existsSync(fixesPath) ? JSON.parse(fs.readFileSync(fixesPath, 'utf8')) : {};
  await Q.mapPool(lessons, 1, async (batch) => {
    const L = batch[0];
    if (!L.raw) { console.error(`L${L.lesson} 缺 raw 段，先重跑 parse`); process.exit(1); }
    const convRaw = String(L.raw.conversation || '').replace(/^#.*$/gm, '').trim().slice(0, 12000);
    const gramRaw = String(L.raw.grammar || '').replace(/^#.*$/gm, '').trim().slice(0, 12000);
    const log = [];
    let conv = [], gram = [];

    if (convRaw) {
      const r1 = await Q.llm(CONV_PROMPT(convRaw), 4);
      if (!Array.isArray(r1)) { console.error(`L${L.lesson} 会话还原失败`); process.exit(1); }
      const v1 = r1.length ? await Q.llm(VERIFY_PROMPT(convRaw, r1, '会话/课文'), 4) : r1;
      if (!Array.isArray(v1)) { console.error(`L${L.lesson} 会话核对失败`); process.exit(1); }
      log.push(`conv初=${r1.length} 核=${v1.length}`);
      conv = cleanList(v1, 'conversation', log);
    }
    if (gramRaw) {
      const r2 = await Q.llm(GRAM_PROMPT(gramRaw), 4);
      if (!Array.isArray(r2)) { console.error(`L${L.lesson} 语法例句还原失败`); process.exit(1); }
      const v2 = r2.length ? await Q.llm(VERIFY_PROMPT(gramRaw, r2, '语法例句'), 4) : r2;
      if (!Array.isArray(v2)) { console.error(`L${L.lesson} 语法例句核对失败`); process.exit(1); }
      log.push(`gram初=${r2.length} 核=${v2.length}`);
      gram = cleanList(v2, 'grammar', log);
    }

    const rawPool = conv.concat(gram);
    // 会话与语法例句可能撞同一句：全局去重，避免同一词下重复绑定（qa_parse 会误判 dup 隔离）
    const seenPool = new Set();
    const pool0 = rawPool.filter(s => { if (seenPool.has(s.text)) return false; seenPool.add(s.text); return true; });
    let fixed = 0;
    const pool = pool0.flatMap(s => {
      const f = fixes[s.text];
      if (!f) return [s];
      fixed++;
      return (Array.isArray(f) ? f : [f]).map(x => ({ ...s, ...x }));
    });
    if (fixed) log.push(`修订=${fixed}`);
    fs.writeFileSync(path.join(cleanDir, `L${L.lesson}.json`), JSON.stringify(pool, null, 1));
    if (!pool.length) { console.error(`L${L.lesson} 还原结果为空`); process.exit(1); }

    // 重新绑定：词面或读音出现在句子里（与 parse 的规则一致）
    for (const c of L.categories) {
      for (const w of c.words) {
        w.sentences = pool.filter(s =>
          (w.surface && s.text.includes(w.surface)) ||
          (w.reading && w.reading.length > 1 && s.text.includes(w.reading))
        ).map(s => ({ text: s.text, ruby: [], translation: s.translation, src: s.src }));
      }
    }
    L.sentencePool = pool.length;
    L.unbound = L.categories.reduce((n, c) => n + c.words.filter(w => !w.sentences.length).length, 0);
    L.cleanStats = log.join(' ');
    console.log(`L${L.lesson}: ${L.cleanStats} 句=${pool.length} 无句词=${L.unbound}`);
  }, 3);

  Q.saveCorpus(fullFile, d);
  // 全量统计
  let uniq = new Set(), unbound = new Set();
  for (const L of d.lessons) {
    for (const c of L.categories) for (const w of c.words) {
      if (!w.sentences.length) unbound.add(w.surface + '|' + (w.reading || ''));
      for (const s of w.sentences) uniq.add(s.text);
    }
  }
  console.log(`完成: 去重句=${uniq.size} 无句词=${unbound.size} ${only ? '(only L' + only + ')' : ''}`);
  console.log(`per-lesson 还原留档: ${cleanDir}`);
})();
