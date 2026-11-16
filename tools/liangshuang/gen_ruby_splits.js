// 生成 web/js/ruby-splits.js：按字读音表。
// 输入：本地库所有注音来源（生词表、生词本例句、课程句、语法例句、课文）；
// 流程：提取核心汉字串 → LLM 逐字拆读音 → 程序校验拼接回原读音 → LLM 复核 → 输出。
// 拆不开/复核不通过/不确定的词不入表，前端回落整词注音（宁整勿错）。
// 用法：node tools/liangshuang/gen_ruby_splits.js
const fs = require('fs');
const path = require('path');
const { execFileSync } = require('child_process');
const Q = require('./qa_lib.js');

const PSQL = process.env.PSQL_BIN || 'G:/TingShuo/pgsql/bin/psql';
const OUT = path.resolve(__dirname, '../../web/js/ruby-splits.js');
const LOG = 'E:/tmp/ruby-gen.log';
const BATCH = 40;
const CONC = 4;

const log = m => {
  const line = `${new Date().toISOString().slice(11, 19)} ${m}`;
  fs.appendFileSync(LOG, line + '\n');
  console.log(line);
};

// 本地 LLM 调用：比 qa_lib 的超时短（正常 40 条约 10 秒），单批卡住不会拖死整轮
async function llm(prompt, tries = 2, timeoutMs = 60000) {
  for (let t = 0; t < tries; t++) {
    try {
      const ac = new AbortController();
      const to = setTimeout(() => ac.abort(), timeoutMs);
      const r = await fetch(Q.BASE + '/chat/completions', {
        signal: ac.signal, method: 'POST',
        headers: { 'Content-Type': 'application/json', Authorization: 'Bearer ' + Q.env.LLM_API_KEY },
        body: JSON.stringify({
          model: Q.env.LLM_MODEL,
          messages: [{ role: 'user', content: prompt }],
          thinking: { type: 'disabled' },
        }),
      });
      clearTimeout(to);
      if (!r.ok) throw new Error('HTTP ' + r.status);
      const j = await r.json();
      const raw = Q.largestJSON(j.choices?.[0]?.message?.content || '');
      if (!raw) throw new Error('no json');
      return JSON.parse(raw);
    } catch (e) {
      if (t === tries - 1) { log(`LLM fail: ${e.message}`); return null; }
      await new Promise(r => setTimeout(r, 1200));
    }
  }
}

const SQL = `
WITH allpairs AS (
  SELECT surface AS s, reading AS r FROM word
  WHERE reading IS NOT NULL AND reading <> surface
  UNION ALL
  SELECT t->>0, t->>1 FROM notebook_sentence, jsonb_array_elements(ruby_tokens) t
  WHERE t->>1 IS NOT NULL AND t->>1 <> t->>0
  UNION ALL
  SELECT t->>0, t->>1 FROM sentence, jsonb_array_elements(ruby_tokens) t
  WHERE t->>1 IS NOT NULL AND t->>1 <> t->>0
  UNION ALL
  SELECT t->>0, t->>1 FROM grammar_example, jsonb_array_elements(ruby_tokens) t
  WHERE t->>1 IS NOT NULL AND t->>1 <> t->>0
  UNION ALL
  SELECT t->>0, t->>1 FROM recitation_line, jsonb_array_elements(ruby_tokens) t
  WHERE t->>1 IS NOT NULL AND t->>1 <> t->>0
)
SELECT s, r, count(*) FROM allpairs GROUP BY s, r`;

const KANJI = /[\u4E00-\u9FFF]/;
const HIRA = /^[\u3041-\u3096\u30FC]+$/;
const isKana = c => /[\u3041-\u3096\u30A1-\u30FA]/.test(c);

// 与前端 rubyCore 完全同一套：剥掉与读音同形的假名前缀/后缀，只留核心
function coreOf(s, r) {
  const sr = Array.from(s), rr = Array.from(r);
  if (!sr.length || !rr.length) return null;
  let p = 0;
  while (p < sr.length && p < rr.length && sr[p] === rr[p] && isKana(sr[p])) p++;
  let suf = 0;
  while (suf < sr.length - p && suf < rr.length - p
    && sr[sr.length - 1 - suf] === rr[rr.length - 1 - suf]
    && isKana(sr[sr.length - 1 - suf])) suf++;
  const cs = sr.slice(p, sr.length - suf).join('');
  const cr = rr.slice(p, rr.length - suf).join('');
  if (!cs || !cr) return null;
  // 只处理纯汉字或汉字夹平假名（无 々/片假名/数字/标点）；至少两个汉字
  if (!/^[\u4E00-\u9FFF]+(?:[\u3041-\u3096]+[\u4E00-\u9FFF]+)*$/.test(cs)) return null;
  if (!HIRA.test(cr)) return null;
  if (Array.from(cs).filter(c => KANJI.test(c)).length < 2) return null;
  if (Array.from(cs).length > 12) return null;
  return { s: cs, r: cr };
}

function loadCandidates() {
  const raw = execFileSync(PSQL, ['-t', '-A', '-F', '\x01', '-d', Q.env.DATABASE_URL, '-c', SQL],
    { maxBuffer: 1 << 28 }).toString('utf8');
  const seen = new Map();
  for (const line of raw.split('\n')) {
    if (!line.trim()) continue;
    const [s0, r0, n0] = line.replace(/\r$/, '').split('\x01');
    const s = (s0 || '').trim();
    const r = (r0 || '').trim();
    const c = coreOf(s, r);
    if (!c) continue;
    const k = c.s + '|' + c.r;
    const n = Number(n0) || 0;
    if (seen.has(k)) seen.get(k).n += n;
    else seen.set(k, { ...c, n });
  }
  return [...seen.values()];
}

// 程序校验：每个汉字单独一项、词语与读音拼回必须与输入完全一致；假名项必须是平假名且无读音
function validate(cs, cr, split) {
  if (!Array.isArray(split) || split.length < 2) return null;
  let ss = '', rr = '', kanji = 0;
  const parts = [];
  for (const item of split) {
    if (!Array.isArray(item) || item.length !== 2) return null;
    const t = item[0];
    let rt = item[1];
    if (typeof t !== 'string' || !t) return null;
    if (rt === undefined) rt = null;
    if (rt !== null && typeof rt !== 'string') return null;
    if (rt === '') rt = null;
    const nk = Array.from(t).filter(c => KANJI.test(c)).length;
    if (nk > 0) {
      if (nk !== 1 || Array.from(t).length !== 1) return null; // 逐字：汉字项只能是一个汉字
      if (!rt || !HIRA.test(rt)) return null;
      kanji++;
    } else {
      if (rt !== null) return null; // 假名项无读音
      if (!/^[\u3041-\u3096]+$/.test(t)) return null;
    }
    ss += t;
    rr += rt || '';
    parts.push([t, rt]);
  }
  if (ss !== cs || rr !== cr || kanji < 2) return null;
  return parts;
}

const splitPrompt = lines => `你是日语教材的注音拆分器。输入是日语词的核心部分（可能含词中假名）和它的平假名读音；请判断能否把读音按汉字逐字拆开。
规则：
1. 逐字拆：每个汉字单独一项并给它的平假名读音；词中假名原样保留，读音写 null。
   例：苦手/にがて → [["苦","にが"],["手","て"]]；日本語/にほんご → [["日","に"],["本","ほん"],["語","ご"]]；売り場/うりば → [["売","う"],["り",null],["場","ば"]]
2. 熟字訓、整体读音、连浊或音变导致单字读音不成立的，不拆，split 写 null。
   例：今日/きょう、大人/おとな、田舎/いなか、眼鏡/めがね、昨日/きのう、一人/ひとり、素敵/すてき、上手/じょうず。
3. 不确定时一律 null，宁整勿错。
只输出 JSON 数组，每项 {"i":序号,"split":拆分|null}，不要解释。
待拆：
${lines}`;

const verifyPrompt = lines => `下面是日语词按字注音的拆分结果。请核对每个汉字的读音放在这个词里是否成立（有没有拆错、读音错位）。只输出明显错误的序号 JSON 数组；不确定的不要标。全部正确输出 []。
${lines}`;

// 补拆：首轮每批词多时模型偏保守，对未拆出的词再问一次；正反例写死，只把熟字訓/当て字留整词
const repairPrompt = lines => `你是日语教材的注音拆分器。下面的词上一轮没拆开，但其实大多数是规则的音读/训读汉字组合，必须按汉字逐字给出读音（词中假名原样保留、读音 null）。
正例：掃除/そうじ → [["掃","そう"],["除","じ"]]；会社/かいしゃ → [["会","かい"],["社","しゃ"]]；買い物/かいもの → [["買","か"],["い",null],["物","もの"]]；誕生日/たんじょうび → [["誕","たん"],["生","じょう"],["日","び"]]；仕事/しごと → [["仕","し"],["事","ごと"]]
只有熟字訓、当て字、或单字读音不成立的词才写 null：今日/きょう、明日/あした、昨日/きのう、部屋/へや、友達/ともだち、大丈夫/だいじょうぶ、真面目/まじめ、上手/じょうず、下手/へた、土産/みやげ、寿司/すし、風呂/ふろ、梅雨/つゆ、風邪/かぜ、怪我/けが、素敵/すてき、一緒/いっしょ、子供/こども、面白/おもしろ。
只输出 JSON 数组，每项 {"i":序号,"split":拆分|null}，不要解释。
待拆：
${lines}`;

// 熟字訓/当て字：按设计保持整词注音（字面读音不成立或会误导），即使模型给出了"拆分"也不入表
const JUKUJIKUN = [
  ['今日', 'きょう'], ['明日', 'あした'], ['昨日', 'きのう'], ['一昨日', 'おととい'], ['今朝', 'けさ'],
  ['大人', 'おとな'], ['一人', 'ひとり'], ['二人', 'ふたり'], ['田舎', 'いなか'], ['眼鏡', 'めがね'],
  ['友達', 'ともだち'], ['部屋', 'へや'], ['真面目', 'まじめ'], ['土産', 'みやげ'], ['梅雨', 'つゆ'],
  ['風邪', 'かぜ'], ['一緒', 'いっしょ'], ['子供', 'こども'], ['面白', 'おもしろ'], ['怪我', 'けが'],
  ['素敵', 'すてき'], ['上手', 'じょうず'], ['下手', 'へた'], ['大丈夫', 'だいじょうぶ'], ['寿司', 'すし'],
  ['風呂', 'ふろ'], ['歌舞伎', 'かぶき'], ['迷子', 'まいご'], ['従兄弟', 'いとこ'], ['行方', 'ゆくえ'],
  ['草履', 'ぞうり'], ['明日は来', 'あしたはこ'],
];
const isJukujikun = (s, r) => JUKUJIKUN.some(([ws, wr]) => s.startsWith(ws) && r.startsWith(wr));

async function verifyEntries(list, label) {
  const bad = new Set();
  let checked = 0;
  await Q.mapPool(list, BATCH, async (batch) => {
    const lines = batch.map(([k, parts], i) => {
      const [s, r] = k.split('|');
      const shown = parts.map(([t, rt]) => rt ? `${t}=${rt}` : t).join(' ');
      return `${i}. ${s}/${r} → ${shown}`;
    }).join('\n');
    const res = await llm(verifyPrompt(lines));
    if (Array.isArray(res)) {
      for (const n of res) {
        const c = batch[Number(n)];
        if (c) bad.add(c[0]);
      }
    } else {
      log(`  ${label}批量失败，保留该批(首轮结果): ${batch.length} 条`);
    }
    checked += batch.length;
    log(`${label} ${checked}/${list.length} · 剔除 ${bad.size}`);
    return [];
  }, CONC);
  return bad;
}

(async () => {
  fs.writeFileSync(LOG, '');
  const cands = loadCandidates();
  log(`候选核心词块: ${cands.length}`);

  const splits = new Map();
  let done = 0;
  await Q.mapPool(cands, BATCH, async (batch) => {
    const lines = batch.map((c, i) => `${i}. ${c.s}→${c.r}`).join('\n');
    const res = await llm(splitPrompt(lines));
    const arr = Array.isArray(res) ? res : [];
    for (const it of arr) {
      const idx = Number(it && it.i);
      const c = batch[idx];
      if (!c) continue;
      const v = validate(c.s, c.r, it.split);
      if (v) splits.set(c.s + '|' + c.r, v);
    }
    done += batch.length;
    log(`拆字 ${done}/${cands.length} · 拆出 ${splits.size}`);
    return [];
  }, CONC);

  const entries = [...splits.entries()];
  log(`复核 ${entries.length} 条…`);
  const bad = await verifyEntries(entries, '复核');

  // 补拆：首轮没拆出或被复核剔除的词，用正反例更明确的提示再拆一次
  const missing = cands.filter(c => !splits.has(c.s + '|' + c.r) || bad.has(c.s + '|' + c.r));
  log(`补拆 ${missing.length} 条…`);
  const repaired = new Map();
  let rdone = 0;
  await Q.mapPool(missing, BATCH, async (batch) => {
    const lines = batch.map((c, i) => `${i}. ${c.s}→${c.r}`).join('\n');
    const res = await llm(repairPrompt(lines));
    const arr = Array.isArray(res) ? res : [];
    for (const it of arr) {
      const idx = Number(it && it.i);
      const c = batch[idx];
      if (!c) continue;
      const v = validate(c.s, c.r, it.split);
      if (v) repaired.set(c.s + '|' + c.r, v);
    }
    rdone += batch.length;
    log(`补拆 ${rdone}/${missing.length} · 新增 ${repaired.size}`);
    return [];
  }, CONC);

  const rEntries = [...repaired.entries()];
  log(`复核补拆 ${rEntries.length} 条…`);
  const rbad = rEntries.length ? await verifyEntries(rEntries, '复核补拆') : new Set();

  const finalMap = {};
  for (const [k, v] of entries) {
    const [s, r] = k.split('|');
    if (!bad.has(k) && !isJukujikun(s, r)) finalMap[k] = v;
  }
  for (const [k, v] of rEntries) {
    const [s, r] = k.split('|');
    if (!rbad.has(k) && !finalMap[k] && !isJukujikun(s, r)) finalMap[k] = v;
  }
  const keys = Object.keys(finalMap).sort();
  const body = keys.map(k => `  ${JSON.stringify(k)}: ${JSON.stringify(finalMap[k])}`).join(',\n');
  const js = `// 由 tools/liangshuang/gen_ruby_splits.js 生成：按字读音表。
// 键为「核心词|读音」，值为逐字拆分 [[字, 读音|null], ...]；表外词回落整词注音。
window.RUBY_SPLITS = {
${body}
};
`;
  fs.writeFileSync(OUT, js);
  log(`写入 ${OUT} · ${keys.length} 条 · ${(js.length / 1024).toFixed(1)} KB`);
  log(`未拆/剔除: ${cands.length - keys.length}`);

  log('已知词检查:');
  for (const probe of ['苦手|にがて', '日本語|にほんご', '運動|うんどう', '言葉|ことば', '売り場|うりば',
    '食べ物|たべもの', '上手|じょうず', '今日|きょう', '大人|おとな', '田舎|いなか', '眼鏡|めがね',
    '一人|ひとり', '左側|ひだりがわ', '試合|しあい', '旅行日|りょこうび']) {
    log(`  ${probe}: ${finalMap[probe] ? JSON.stringify(finalMap[probe]) : '整词'}`);
  }

  log('抽样(拆出):');
  const step = Math.max(1, Math.floor(keys.length / 15));
  for (let i = 0; i < keys.length; i += step) {
    log(`  ${keys[i]} → ${finalMap[keys[i]].map(([t, rt]) => rt ? `${t}(${rt})` : t).join('')}`);
  }
})().catch(e => { console.error(e); process.exit(1); });
