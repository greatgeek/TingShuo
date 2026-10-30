// QA 关⑥ 导入护栏：pre = 差异报告 + 熔断（句数偏离现存 >30% 中止）；post = 对账（行数/注音填充/译文缺失/指纹）。
// 用法: node qa_import.js <fullJson> pre|post [psqlBin]
const { execFileSync } = require('child_process');
const Q = require('./qa_lib');

const [jp, mode, psqlArg] = process.argv.slice(2);
const PSQL = psqlArg || 'G:/TingShuo/pgsql/bin/psql';
const d = Q.loadCorpus(jp);

const qword = new Set(Q.loadQ().filter(x => ["reading", "hallucinate"].includes(x.reason) && x.surface).map(x => x.surface));
const qset = new Set(Q.loadQ().filter(x => ['bind', 'dup', 'trans', 'trans-empty', 'grammar', 'truncated', 'mixed', 'typo', 'ruby-struct', 'audio-block'].includes(x.reason)).map(x => x.text));
let wordLinks = 0, sentLinks = 0, sents = new Set(), words = new Set();
for (const L of d.lessons) for (const c of L.categories) for (const w of c.words) {
  if (qword.has(w.surface)) continue;
  words.add(w.surface + '|' + (w.reading || ''));
  wordLinks++;
  for (const s of w.sentences) { sentLinks++; if (!qset.has(s.text)) sents.add(s.text); }
}
const sql = (q) => execFileSync(PSQL, ['-U', 'tingshuo', '-d', 'tingshuo', '-t', '-A', '-c', q], { maxBuffer: 1 << 26 }).toString('utf8').trim();

if (mode === 'pre') {
  const dbSent = +sql('SELECT count(*) FROM notebook_sentence');
  const dbLink = +sql('SELECT count(*) FROM notebook_word');
  const dev = dbLink ? Math.abs(wordLinks - dbLink) / dbLink : 0;
  console.log(`JSON: 词=${words.size} 挂载(除隔离)=${wordLinks} 句链接=${sentLinks} 句去重=${sents.size} | DB: 挂载=${dbLink} 句=${dbSent} | 挂载偏离=${(dev * 100).toFixed(1)}%`);
  if (dbSent && dev > 0.3) { console.log('ABORT: 偏离超 30%，疑似解析事故，禁止导入'); process.exit(1); }
  console.log('pre-check OK，可导入');
} else {
  const dbSent = +sql('SELECT count(*) FROM notebook_sentence');
  const nullRuby = +sql('SELECT count(*) FROM notebook_sentence WHERE ruby_tokens IS NULL');
  const noTrans = +sql("SELECT count(*) FROM notebook_sentence WHERE translation IS NULL OR translation = ''");
  const fp = sql('SELECT md5(string_agg(text, chr(124) ORDER BY id)) FROM notebook_sentence');
  console.log(`post: DB句=${dbSent} JSON句链接=${sentLinks} 差=${dbSent - sentLinks} | rubyNULL=${nullRuby} 空译文=${noTrans} | 指纹=${fp}`);
  if (nullRuby || noTrans) { console.log('WARN: 存在未回填注音或空译文'); process.exit(2); }
}
