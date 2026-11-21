// 课文本对齐公共库：归一化、编辑距离、教材句子→SRT cue 区间（DP 全局对齐）
const norm = s => String(s || '')
  .normalize('NFKC').toLowerCase()
  .replace(/[\s\u3000]+/g, '')
  .replace(/[、。，．！？!?,.・「」『』（）()\[\]【】〈〉《》…‥:：;；~〜\-—–]/g, '')
  .replace(/[\u30A1-\u30F6]/g, c => String.fromCharCode(c.charCodeAt(0) - 0x60));

function lev(a, b) {
  if (a === b) return 0;
  if (!a.length) return b.length;
  if (!b.length) return a.length;
  let prev = Array.from({ length: b.length + 1 }, (_, i) => i);
  for (let i = 1; i <= a.length; i++) {
    const cur = [i];
    for (let j = 1; j <= b.length; j++) {
      cur[j] = Math.min(prev[j] + 1, cur[j - 1] + 1, prev[j - 1] + (a[i - 1] === b[j - 1] ? 0 : 1));
    }
    prev = cur;
  }
  return prev[b.length];
}
const sim = (a, b) => 1 - lev(a, b) / Math.max(a.length, b.length, 1);

function alignLesson(lines, cues) {
  const T = lines.map(s => norm(s.text));
  const C = cues.map(c => norm(c.text));
  const n = T.length, m = C.length;
  const skipLineCost = 0.55, skipCueCost = 0.002;
  const memo = new Map();
  const choice = new Map();
  function spanBest(i, j) {
    const ln = T[i];
    if (ln.length < 2) return null;
    let best = null;
    const maxA = Math.min(m - 1, j + 30);
    for (let a = j; a <= maxA; a++) {
      if (!C[a]) continue;
      const maxSpan = Math.min(m - 1, a + Math.max(3, Math.ceil(ln.length / 8) + 3));
      let acc = '';
      for (let b = a; b <= maxSpan; b++) {
        acc += C[b];
        if (acc.length > ln.length * 2.2 + 10) break;
        const sc = sim(ln, acc) - 0.002 * (b - a);
        if (!best || sc > best.sc + 1e-9) best = { a, b, sc };
      }
    }
    return best;
  }
  function dp(i, j) {
    if (i >= n) return 0;
    if (j >= m) return -skipLineCost * (n - i);
    const key = i * (m + 1) + j;
    if (memo.has(key)) return memo.get(key);
    let best = -Infinity, how = null;
    const sb = spanBest(i, j);
    if (sb && sb.sc > -1) {
      const v = sb.sc + dp(i + 1, sb.b + 1);
      if (v > best) { best = v; how = { t: 'match', a: sb.a, b: sb.b, sc: sb.sc }; }
    }
    const vSkipLine = -skipLineCost + dp(i + 1, j);
    if (vSkipLine > best) { best = vSkipLine; how = { t: 'skipLine' }; }
    const vSkipCue = -skipCueCost + dp(i, j + 1);
    if (vSkipCue > best) { best = vSkipCue; how = { t: 'skipCue' }; }
    memo.set(key, best); choice.set(key, how);
    return best;
  }
  dp(0, 0);
  const out = [];
  let i = 0, j = 0;
  while (i < n) {
    if (j >= m) {
      out.push({ ...lines[i], start: null, end: null, score: 0, note: 'NO_CUE' });
      i++;
      continue;
    }
    const how = choice.get(i * (m + 1) + j);
    if (!how) { out.push({ ...lines[i], start: null, end: null, score: 0, note: 'NO_PATH' }); i++; continue; }
    if (how.t === 'skipLine') {
      out.push({ ...lines[i], start: null, end: null, score: 0, note: 'NO_MATCH' });
      i++;
    } else if (how.t === 'skipCue') {
      j++;
    } else {
      const start = Math.max(0, Math.round((cues[how.a].start - 0.06) * 1000) / 1000);
      const end = Math.round((cues[how.b].end + 0.08) * 1000) / 1000;
      out.push({
        text: lines[i].text, translation: lines[i].translation || '',
        start, end, score: Math.round(how.sc * 1000) / 1000,
        cues: [how.a + 1, how.b + 1],
        spanText: cues.slice(how.a, how.b + 1).map(c => c.text).join(''),
      });
      i++; j = how.b + 1;
    }
  }
  let prevEnd = 0;
  for (const l of out) {
    if (l.start == null) continue;
    if (l.start < prevEnd) l.start = prevEnd;
    prevEnd = l.end;
  }
  return out;
}

module.exports = { norm, lev, sim, alignLesson };
