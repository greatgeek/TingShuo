// QA 关⑥ 导入护栏（增量按教材目录树核算）：
// pre = 已有本教材子树时做差异熔断（偏离 >30% 中止），新建时报告基线；
// post = 子树行数精确对账 + 注音填充 + 译文缺失 + 指纹。
// 用法: node qa_import.js <fullJson> pre|post [psqlBin]
const { execFileSync } = require('child_process');
const Q = require('./qa_lib');

const [jp, mode, psqlArg] = process.argv.slice(2);
const PSQL = psqlArg || 'G:/TingShuo/pgsql/bin/psql';
const d = Q.loadCorpus(jp);
const LEVEL = d.level1 || '初级上';
const ROOT_DEF = {
  '初级上': 'nbd-cjs', '初级下': 'nbd-cjs-xia',
  '中级上': 'nbd-cjs-zs', '中级下': 'nbd-cjs-zx', '高级': 'nbd-cjs-gj',
};
const ROOT = ROOT_DEF[LEVEL] || 'nbd-cjs';

const qword = new Set(Q.loadQ().filter(x => ["reading", "hallucinate"].includes(x.reason) && x.surface).map(x => x.surface));
const qset = new Set(Q.loadQ().filter(x => ['bind', 'dup', 'trans', 'trans-empty', 'grammar', 'truncated', 'mixed', 'typo', 'ruby-struct', 'audio-block'].includes(x.reason)).map(x => x.text));
let wordLinks = 0, sentLinks = 0;
const sents = new Set(), words = new Set();
for (const L of d.lessons) for (const c of L.categories) for (const w of c.words) {
  if (qword.has(w.surface)) continue;
  words.add(w.surface + '|' + (w.reading || ''));
  wordLinks++;
  for (const s of w.sentences) {
    if (qset.has(s.text)) continue;
    sentLinks++;
    sents.add(s.text);
  }
}
const sql = (q) => execFileSync(PSQL, ['-U', 'tingshuo', '-d', 'tingshuo', '-t', '-A', '-c', q], { maxBuffer: 1 << 26 }).toString('utf8').trim();
const SUB = `WITH RECURSIVE sub AS (
  SELECT id FROM notebook_dir WHERE id = '${ROOT}'
  UNION ALL
  SELECT nd.id FROM notebook_dir nd JOIN sub s ON nd.parent_id = s.id
)`;

if (mode === 'pre') {
  const subLink = +sql(`${SUB} SELECT count(*) FROM notebook_word nw JOIN sub ON sub.id = nw.dir_id`);
  const subSent = +sql(`${SUB} SELECT count(*) FROM notebook_sentence ns JOIN notebook_word nw ON nw.id = ns.notebook_word_id JOIN sub ON sub.id = nw.dir_id`);
  const dbLink = +sql('SELECT count(*) FROM notebook_word');
  const dbSent = +sql('SELECT count(*) FROM notebook_sentence');
  console.log(`JSON(${LEVEL}): 词=${words.size} 挂载(除隔离)=${wordLinks} 句链接=${sentLinks} 句去重=${sents.size}`);
  console.log(`DB 基线: 全库挂载=${dbLink} 全库句=${dbSent} | 本教材子树: 挂载=${subLink} 句=${subSent}`);
  if (subLink) {
    const dev = Math.abs(wordLinks - subLink) / subLink;
    console.log(`已有子树，挂载偏离=${(dev * 100).toFixed(1)}%`);
    if (dev > 0.3) { console.log('ABORT: 偏离超 30%，疑似解析事故，禁止导入'); process.exit(1); }
  } else {
    console.log('本教材子树为空（首次增量导入），以全库基线为参照');
  }
  console.log('pre-check OK，可导入');
} else {
  const subLink = +sql(`${SUB} SELECT count(*) FROM notebook_word nw JOIN sub ON sub.id = nw.dir_id`);
  const subSent = +sql(`${SUB} SELECT count(*) FROM notebook_sentence ns JOIN notebook_word nw ON nw.id = ns.notebook_word_id JOIN sub ON sub.id = nw.dir_id`);
  const nullRuby = +sql(`${SUB} SELECT count(*) FROM notebook_sentence ns JOIN notebook_word nw ON nw.id = ns.notebook_word_id JOIN sub ON sub.id = nw.dir_id WHERE ns.ruby_tokens IS NULL`);
  const noTrans = +sql(`${SUB} SELECT count(*) FROM notebook_sentence ns JOIN notebook_word nw ON nw.id = ns.notebook_word_id JOIN sub ON sub.id = nw.dir_id WHERE ns.translation IS NULL OR ns.translation = ''`);
  const fp = sql(`${SUB} SELECT md5(string_agg(ns.text, chr(124) ORDER BY ns.id)) FROM notebook_sentence ns JOIN notebook_word nw ON nw.id = ns.notebook_word_id JOIN sub ON sub.id = nw.dir_id`);
  const dbLink = +sql('SELECT count(*) FROM notebook_word');
  const dbSent = +sql('SELECT count(*) FROM notebook_sentence');
  console.log(`post(${LEVEL}): 子树挂载=${subLink}(JSON ${wordLinks}) 子树句=${subSent}(JSON ${sentLinks}) 差=${subSent - sentLinks}`);
  console.log(`全库: 挂载=${dbLink} 句=${dbSent} | rubyNULL=${nullRuby} 空译文=${noTrans} | 指纹=${fp}`);
  if (subLink !== wordLinks || subSent !== sentLinks) { console.log('FAIL: 子树行数与 JSON 不一致'); process.exit(2); }
  if (nullRuby || noTrans) { console.log('FAIL: 存在未回填注音或空译文'); process.exit(2); }
  console.log('post-check OK');
}
