# -*- coding: utf-8 -*-
# 用 SRT 转写时间轴对齐 content.py 的听力内容，产出 lessons/lNN.json
import re, os, sys, json, difflib, subprocess
sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
from content import L

SRC = r"//DXP4800-F444/openclaw/合集/清华梁爽：0-N1日语精讲高级班"
OUT = r"E:/听说app/TingShuo/tools/listening/lessons"
INSTR_MARKS = ("录音", "填写", "选择", "判断", "数字", "括号", "回答", "内容", "例", "画出", "画")


def parse_srt(p):
    raw = open(p, encoding='utf-8-sig').read().replace('\r', '')
    cues = []
    for block in re.split(r'\n\n+', raw.strip()):
        ls = block.split('\n')
        if len(ls) < 2:
            continue
        m = re.match(r'(\d+):(\d+):(\d+),(\d+) --> (\d+):(\d+):(\d+),(\d+)', ls[1])
        if not m:
            continue
        g = lambda a, b, c, d: int(a) * 3600000 + int(b) * 60000 + int(c) * 1000 + int(d)
        cues.append(dict(st=g(*m.groups()[:4]), en=g(*m.groups()[4:]), t=''.join(ls[2:]).strip()))
    return cues


def norm(s):
    return re.sub(r'[\s。、，．,.!！?？「」『』（）()・…ー〜~\-]', '', s)


def sim(a, b):
    a, b = norm(a), norm(b)
    if not a or not b:
        return 0.0
    return difflib.SequenceMatcher(None, a, b).ratio()


def is_instruction(cue, instruction):
    t = cue['t']
    if len(t) < 5:
        return False
    if not any(m in t for m in INSTR_MARKS):
        return False
    return sim(t, instruction) >= 0.35


def split_groups(cues, n):
    """把 cues 切成 n 段，段跨度不小于阈值，最大化切点静音间隙总和。"""
    m = len(cues)
    if n <= 1:
        return [cues]
    for min_span in (7000, 5000, 3000, 1000, 0):
        NEG = -1e18
        dp = [[NEG] * (m + 1) for _ in range(n + 1)]
        bk = [[0] * (m + 1) for _ in range(n + 1)]
        dp[0][0] = 0
        for k in range(1, n + 1):
            for i in range(1, m + 1):
                for j in range(k - 1, i):
                    if dp[k - 1][j] == NEG:
                        continue
                    span = cues[i - 1]['en'] - cues[j]['st']
                    if k == n:
                        if i != m:
                            continue
                    else:
                        if i == m:
                            continue
                    if span < min_span and not (k == n and j == k - 1):
                        continue
                    gain = 0 if j == 0 else (cues[j]['st'] - cues[j - 1]['en'])
                    v = dp[k - 1][j] + gain
                    if v > dp[k][i]:
                        dp[k][i] = v
                        bk[k][i] = j
        if dp[n][m] != NEG:
            out = []
            i = m
            for k in range(n, 0, -1):
                j = bk[k][i]
                out.append(cues[j:i])
                i = j
            return out[::-1]
    raise RuntimeError('无法分段')


def alloc_lines(cues, lines):
    """段内把 cues 连续分配给 lines：文本相似度 + 时长先验 DP。"""
    m, n = len(lines), len(cues)
    if m == 0:
        return []
    if n == 0 or n < m:
        span_s, span_e = cues[0]['st'], cues[-1]['en']
        total = sum(max(len(l[2]), 4) for l in lines)
        cur, out = span_s, []
        for l in lines:
            d = (span_e - span_s) * max(len(l[2]), 4) / total
            out.append((int(cur), int(cur + d)))
            cur += d
        return out
    chars = [max(len(l[2]), 4) for l in lines]
    total = sum(chars)
    span = cues[-1]['en'] - cues[0]['st']
    exp = [span * c / total for c in chars]
    NEG = 1e18
    dp = [[NEG] * (n + 1) for _ in range(m + 1)]
    bk = [[0] * (n + 1) for _ in range(m + 1)]
    dp[0][0] = 0
    for k in range(1, m + 1):
        for i in range(k, n + 1):
            for j in range(k - 1, i):
                if dp[k - 1][j] >= NEG / 2:
                    continue
                seg = cues[j:i]
                txt = ''.join(c['t'] for c in seg)
                dur = seg[-1]['en'] - seg[0]['st']
                penalty = min(2.0, abs(dur - exp[k - 1]) / max(1.0, exp[k - 1]))
                cost = dp[k - 1][j] + (1 - 1.5 * sim(txt, lines[k - 1][2])) + 0.45 * penalty
                if cost < dp[k][i]:
                    dp[k][i] = cost
                    bk[k][i] = j
    if dp[m][n] >= NEG / 2:
        span_s, span_e = cues[0]['st'], cues[-1]['en']
        cur, out = span_s, []
        for c in chars:
            d = (span_e - span_s) * c / total
            out.append((int(cur), int(cur + d)))
            cur += d
        return out
    segs = []
    i = n
    for k in range(m, 0, -1):
        j = bk[k][i]
        segs.append((j, i))
        i = j
    segs = segs[::-1]
    return [(cues[j]['st'], cues[i - 1]['en']) for j, i in segs]


def build(n, l):
    cues = parse_srt(os.path.join(SRC, l['srt']))
    last_end = cues[-1]['en'] if cues else 0
    lesson = dict(id=f"listen-{n:02d}", lesson_no=n, title=l['title'],
                  audio_key=f"listening/listening-lesson-{n:02d}.mp3",
                  duration_ms=0, sort_order=0, sections=[])
    for s in l['sections']:
        a, b = s['cue']
        seg = cues[a - 1:b]
        if seg and is_instruction(seg[0], s['instruction']):
            seg = seg[1:]
        if not seg:
            raise RuntimeError(f'L{n} s{s["no"]} 无内容 cue')
        raw_lines = s['lines']
        grouped = len(raw_lines) and len(raw_lines[0]) == 4
        out_lines = []
        if grouped:
            items = sorted(set(x[0] for x in raw_lines))
            groups = split_groups(seg, len(items))
            assert len(groups) == len(items), (n, s['no'], len(groups), len(items))
            for it, grp in zip(items, groups):
                ls = [x for x in raw_lines if x[0] == it]
                times = alloc_lines(grp, ls)
                for x, (st, en) in zip(ls, times):
                    out_lines.append((x[1], st, en, x[2], x[3]))
        else:
            times = alloc_lines(seg, raw_lines)
            for x, (st, en) in zip(raw_lines, times):
                out_lines.append((x[0], st, en, x[1], x[2]))
        qs = []
        for q in s['questions']:
            o = []
            for k, t in q.get('options', []):
                o.append(dict(key=k, text=t))
            qs.append(dict(prompt=q['prompt'], options=o, answer=q['answer']))
        sec = dict(section_no=s['no'], kind=s['kind'], instruction=s['instruction'],
                   audio_start_ms=max(0, seg[0]['st'] - 300),
                   audio_end_ms=min(last_end, seg[-1]['en'] + 700),
                   example=(dict(prompt=s['example']['prompt'], answer=s['example']['answer']) if s.get('example') else None),
                   questions=qs,
                   lines=[dict(speaker=sp, start_ms=st, end_ms=en, text=txt, translation=zh)
                          for sp, st, en, txt, zh in out_lines])
        lesson['sections'].append(sec)
    return lesson


def probe_ms(p):
    r = subprocess.run(['ffprobe', '-v', 'error', '-show_entries', 'format=duration', '-of', 'csv=p=0', p],
                       capture_output=True, text=True)
    return int(round(float(r.stdout.strip()) * 1000))


only = [int(x) for x in sys.argv[1:]] or sorted(L)
os.makedirs(OUT, exist_ok=True)
for n in only:
    lesson = build(n, L[n])
    mp4 = os.path.join(SRC, L[n]['srt'].replace('.srt', '.mp4'))
    lesson['duration_ms'] = probe_ms(mp4)
    p = os.path.join(OUT, f'l{n:02d}.json')
    json.dump(lesson, open(p, 'w', encoding='utf-8'), ensure_ascii=False, indent=2)
    print(f"L{n}: sections={len(lesson['sections'])} dur={lesson['duration_ms']} lines={sum(len(s['lines']) for s in lesson['sections'])} q={sum(len(s['questions']) for s in lesson['sections'])}")
    for s in lesson['sections']:
        print(f"  s{s['section_no']} {s['kind']} {s['audio_start_ms']/1000:.1f}-{s['audio_end_ms']/1000:.1f}s "
              f"lines={len(s['lines'])} q={len(s['questions'])} firstline={s['lines'][0]['start_ms']/1000:.1f}")