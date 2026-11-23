package wordstudy

// 背词 quiz 载荷：模式开关决定"这次用哪条通道测"，单词仍是调度单元。
// 标准模式轮换 看词选义/听词选义；听力模式轮换 听句挖空选词/听句挖空输入。
// 挖空卡正面只给语境不给答案：目标词位置空白且不带振假名（注音即答案），
// 语境提示=句子翻译里把词义替换成○○的模糊版，替换不了就不给提示（宁缺勿泄）。

import (
	"context"
	"encoding/json"
	"math/rand"
	"net/url"
	"regexp"
	"strings"

	"github.com/jackc/pgx/v5/pgxpool"
)

type quizType string

// patternMark 句式条目占位符（〜さん/…/○）：这类"词"念出来不明所以，
// 不能进听力通道（2026-11-06 用户报障"〜さん 在听力句子里不明所以"）。
var patternMarkRe = regexp.MustCompile(`[〜~…○□?？]`)

func patternWord(s string) bool { return patternMarkRe.MatchString(s) }

const (
	qFlip      quizType = "flip"       // 兜底：原翻面卡（无合适选项/句子时）
	qPickMean  quizType = "pick_mean"  // 看词选义
	qHearMean  quizType = "hear_mean"  // 听词选义
	qClozePick quizType = "cloze_pick" // 听句挖空选词
	qClozeType quizType = "cloze_type" // 听句挖空输入
)

// Mode 一级模式：standard / listening（口语位预留，首版不显示）。
type Mode string

const (
	ModeStandard  Mode = "standard"
	ModeListening Mode = "listening"
)

func parseMode(s string) Mode {
	if s == string(ModeListening) {
		return ModeListening
	}
	return ModeStandard
}

type sentAudio struct {
	Kind string          `json:"kind"` // asset=audio_asset；say=/audio/say 合成；url=原声片段
	ID   string          `json:"id,omitempty"`
	Text string          `json:"text,omitempty"`
	Ruby json.RawMessage `json:"ruby,omitempty"` // say 时传注音，纠多音字误读
	URL  string          `json:"url,omitempty"`  // url 时同源媒体地址
	Alt  string          `json:"alt,omitempty"`  // 同源缓存不可用时的 CDN 回退
}

type sentToken struct {
	Text    string  `json:"text"`
	Reading *string `json:"reading"`
	Blank   bool    `json:"blank,omitempty"`
}

type Quiz struct {
	Type      quizType    `json:"type"`
	Options   []string    `json:"options,omitempty"` // pick_mean/hear_mean=释义；cloze_pick=词面
	Answer    int         `json:"answer"`            // Options 下标
	Tokens    []sentToken `json:"tokens,omitempty"`  // 挖空句逐 token（空白 token 无读音）
	Hint      *string     `json:"hint"`              // 模糊化句子翻译；nil=本卡不提供提示
	Trans     *string     `json:"trans,omitempty"`   // 背面第二层：完整句子翻译
	Audio     *sentAudio  `json:"audio,omitempty"`
	BlankText string      `json:"blank_text,omitempty"` // 空白处正解显示文本（句式条目=句式部分）
	Accept    []string    `json:"accept,omitempty"`     // 输入题可接受答案（词面/读音/句式部分）
	SentKey   string      `json:"sent_key,omitempty"`   // 本次例句标识，答完记入轮换状态
}

// tokSpan 词元在句中的字节区间；ruby_tokens 必须与正文逐字对齐。
type tokSpan struct {
	Text    string
	Reading *string
	Start   int
	End     int
}

func tokenSpans(raw json.RawMessage, text string) []tokSpan {
	// ruby_tokens 落库形态是 [[text, reading|null], ...] 的二元组数组
	var src [][2]*string
	if err := json.Unmarshal(raw, &src); err != nil || len(src) == 0 {
		return nil
	}
	out := make([]tokSpan, 0, len(src))
	off := 0
	for _, p := range src {
		if p[0] == nil {
			continue
		}
		s := *p[0]
		out = append(out, tokSpan{Text: s, Reading: p[1], Start: off, End: off + len(s)})
		off += len(s)
	}
	var joined strings.Builder
	for _, t := range out {
		joined.WriteString(t.Text)
	}
	if off == 0 || joined.String() != text { // 对齐校验：偏移对不上就不出题
		return nil
	}
	return out
}

const (
	scoreBad     = 0 // 读音对不上：这里的词面不是目标词
	scoreUnknown = 1 // 读音缺失无法判断，沿用旧行为
	scoreCompat  = 2 // 读音能切出，或词面跨词元整体挖空
	scoreExact   = 3 // 词面正好是一个独立词元
)

// blankTokens 把词面在句中的位置换成单个空白 token 且剥掉读音。
// 只空白目标词本身：与目标词重叠的 token 若还有词外文字（言語学習 里的 言語、
// 李さん 里的 李），这些文字照常显示——否则揭晓时整块被词面替换，句子会缺字，
// 听起来的整句发音就和屏幕对不上（2026-11-16「学習にも便利です」报障）。
// 词面在句中可能出现多次：先按读音挑出真正的目标词那一处，再挖空。
// 若只在读音对不上的复合词里出现（味噌汁 的 味、体調 的 体），直接放弃出题，
// 否则音频在空白处念的是另一个词（2026-11-20「この味噌汁の味」报障）。
func blankTokens(raw json.RawMessage, text, surface, reading string) []sentToken {
	tokens, _ := blankTokensScored(raw, text, surface, reading)
	return tokens
}

// blankTokensScored 同时给出本次挖空的可信度，供跨句挑选最优句子。
func blankTokensScored(raw json.RawMessage, text, surface, reading string) ([]sentToken, int) {
	toks := tokenSpans(raw, text)
	if toks == nil || surface == "" {
		return nil, scoreBad
	}
	bestScore, best := -1, []sentToken(nil)
	for off := 0; off+len(surface) <= len(text); {
		start := strings.Index(text[off:], surface)
		if start < 0 {
			break
		}
		start += off
		end := start + len(surface)
		if sc := occurrenceScore(toks, start, end, reading); sc > bestScore {
			bestScore, best = sc, blankAt(toks, start, end, surface, reading)
		}
		if bestScore == scoreExact {
			break
		}
		off = start + len(surface)
	}
	if bestScore <= scoreBad || best == nil {
		return nil, scoreBad
	}
	return best, bestScore
}

// occurrenceScore 判断某一次词面出现是不是"真正的目标词"：
// 独立词元最可靠；词元更长时看读音能否切出（李さん/言語学習 可切，味噌汁 切不出 あじ）。
func occurrenceScore(toks []tokSpan, start, end int, wordReading string) int {
	var overlap []tokSpan
	for _, t := range toks {
		if t.End > start && t.Start < end {
			overlap = append(overlap, t)
		}
	}
	switch len(overlap) {
	case 0:
		return scoreBad
	case 1:
		t := overlap[0]
		switch {
		case t.Start == start && t.End == end:
			return scoreExact
		case t.Start == start && t.End > end:
			return readingScore(t.Reading, wordReading, true, false)
		case t.Start < start && t.End == end:
			return readingScore(t.Reading, wordReading, false, false)
		case t.Start < start && t.End > end:
			return readingScore(t.Reading, wordReading, false, true)
		}
		return scoreUnknown
	default:
		return scoreCompat // 词面跨词元：整体挖空
	}
}

// readingScore 词元读音能否切出目标词读音。两端/中段分别比对；
// 读音缺失或含非假名（英文读音等）时无法判断，返回 scoreUnknown。
func readingScore(tokenReading *string, wordReading string, prefix, middle bool) int {
	if tokenReading == nil || *tokenReading == "" || wordReading == "" {
		return scoreUnknown
	}
	tv, wv := kanaVariants(*tokenReading), kanaVariants(wordReading)
	if len(tv) == 0 || len(wv) == 0 {
		return scoreUnknown
	}
	for _, t := range tv {
		for _, w := range wv {
			var ok bool
			switch {
			case middle:
				ok = strings.Contains(t, w)
			case prefix:
				ok = strings.HasPrefix(t, w)
			default:
				ok = strings.HasSuffix(t, w)
			}
			if ok {
				return scoreCompat
			}
		}
	}
	return scoreBad
}

// kanaVariants 读音归一化：去掉音调符号/括号/空格，按分隔符拆多读音，只留假名。
func kanaVariants(s string) []string {
	out := []string{}
	for _, part := range strings.FieldsFunc(s, func(r rune) bool {
		switch r {
		case '・', '、', ',', '/', '／', ' ', '\u3000', ';', '；', '（', '）', '(', ')', '[', ']':
			return true
		}
		return false
	}) {
		var b strings.Builder
		for _, r := range part {
			if (r >= 0x3040 && r <= 0x309F) || (r >= 0x30A0 && r <= 0x30FF) ||
				(r >= 0x31F0 && r <= 0x31FF) || (r >= 0xFF66 && r <= 0xFF9F) {
				b.WriteRune(r)
			}
		}
		if v := b.String(); v != "" {
			out = append(out, v)
		}
	}
	return out
}

// blankAt 在指定词面区间挖空：词外 token 原样保留，词内 token 并进空白；
// 词面只是词元一部分时保留词外文字，读音能切出时给残留文字振假名。
func blankAt(toks []tokSpan, start, end int, surface, reading string) []sentToken {
	out := []sentToken{}
	blanked := false
	for _, t := range toks {
		if t.End <= start || t.Start >= end { // 词外 token 原样保留
			out = append(out, sentToken{Text: t.Text, Reading: t.Reading})
			continue
		}
		var rd string
		if t.Reading != nil {
			rd = *t.Reading
		}
		// 词左残留（如 言語学習 的 言語）：整词位于本 token 尾部才按读音尾部切分
		if t.Start < start {
			pre := t.Text[:start-t.Start]
			var preR *string
			if t.End == end && t.Text[start-t.Start:] == surface {
				preR = splitReading(rd, reading, true)
			}
			out = append(out, sentToken{Text: pre, Reading: preR})
		}
		if !blanked {
			out = append(out, sentToken{Text: "＿＿＿", Blank: true})
			blanked = true
		}
		// 词右残留（如 李さん 的 さん）：整词位于本 token 头部才按读音头部切分
		if t.End > end {
			suf := t.Text[end-t.Start:]
			var sufR *string
			if t.Start == start && t.Text[:end-t.Start] == surface {
				sufR = splitReading(rd, reading, false)
			}
			out = append(out, sentToken{Text: suf, Reading: sufR})
		}
	}
	if !blanked {
		return nil
	}
	return out
}

// wordReading 词读音，缺省空串（切不出读音时前端就不显示振假名）。
func wordReading(w word) string {
	if w.Reading != nil {
		return *w.Reading
	}
	return ""
}

// splitReading 把整块读音切给空白两侧的残留文字：token 读音必须以词读音结尾
// （前缀侧）或开头（后缀侧）才切；切不准就不给读音，宁缺勿错。
func splitReading(reading, wordReading string, prefix bool) *string {
	if reading == "" || wordReading == "" || reading == wordReading {
		return nil
	}
	rr, wr := []rune(reading), []rune(wordReading)
	if len(rr) <= len(wr) {
		return nil
	}
	var s string
	if prefix {
		if !strings.HasSuffix(reading, wordReading) {
			return nil
		}
		s = string(rr[:len(rr)-len(wr)])
	} else {
		if !strings.HasPrefix(reading, wordReading) {
			return nil
		}
		s = string(rr[len(wr):])
	}
	if s == "" {
		return nil
	}
	return &s
}

// blurHint 句子翻译含词义 → 替换成○○；不含 → nil（前端不显示提示按钮）。
func blurHint(trans, meaning *string) *string {
	if trans == nil || meaning == nil || *meaning == "" {
		return nil
	}
	if !strings.Contains(*trans, *meaning) {
		return nil
	}
	h := strings.Replace(*trans, *meaning, "○○", 1)
	return &h
}

type sentRow struct {
	Src       string
	Key       string // nb:<id> / ls:<id> / wc:<segment_id>
	Text      string
	Ruby      json.RawMessage
	Trans     *string
	Audio     *string // audio_asset id
	ClipAudio string  // 原声 CDN 地址
}

// clipRuby 把 Nadeshiko 的 [{s,r,...}] 转成 ruby_tokens 的 [[text,reading]]。
// 读音统一转平假名；拼接必须与正文逐字一致，否则这条片段不出题。
func clipRuby(raw json.RawMessage, text string) json.RawMessage {
	var src []struct {
		S string `json:"s"`
		R string `json:"r"`
	}
	if json.Unmarshal(raw, &src) != nil || len(src) == 0 {
		return nil
	}
	out := make([][2]*string, 0, len(src))
	var joined strings.Builder
	for _, t := range src {
		if t.S == "" {
			continue
		}
		joined.WriteString(t.S)
		s := t.S
		var rp *string
		if r := kanaReading(t.R); r != "" && r != s && r != kanaReading(s) {
			rr := r
			rp = &rr
		}
		out = append(out, [2]*string{&s, rp})
	}
	if joined.String() != text {
		return nil
	}
	b, err := json.Marshal(out)
	if err != nil {
		return nil
	}
	return b
}

// kanaReading 把片假名读音转平假名；含非假名字符则视为不可用读音。
func kanaReading(s string) string {
	rs := []rune(strings.TrimSpace(s))
	hasKana := false
	for i, r := range rs {
		switch {
		case r >= 0x3041 && r <= 0x3096:
			hasKana = true
		case r >= 0x30A1 && r <= 0x30F6:
			rs[i] = r - 0x60
			hasKana = true
		case r == 0x30FC: // 长音
			hasKana = true
		default:
			return ""
		}
	}
	if !hasKana {
		return ""
	}
	return string(rs)
}

// pickSentences 挖空素材：生词本句优先（有翻译、导入质检过），回退课程句，
// 再回退带中文翻译与音频的原声片段。返回一批候选而不是单条。
func pickSentences(ctx context.Context, pool *pgxpool.Pool, userID string, w word, target string) []sentRow {
	rows, err := pool.Query(ctx, `
		SELECT src, sent_key, text, ruby_tokens, translation, audio_id, clip_tokens, clip_audio FROM (
			SELECT '1nb' AS src, 'nb:' || s.id AS sent_key, s.text,
			       COALESCE(s.ruby_tokens, '[]'::jsonb) AS ruby_tokens, s.translation,
			       NULL::text AS audio_id, '[]'::jsonb AS clip_tokens, ''::text AS clip_audio
			FROM notebook_sentence s
			JOIN notebook_word nw ON nw.id = s.notebook_word_id
			JOIN notebook nb     ON nb.id = nw.notebook_id
			WHERE nw.word_id = $1 AND nb.user_id = $2
			  AND s.translation IS NOT NULL AND s.text LIKE '%' || $3 || '%'
			  AND char_length(s.text) BETWEEN 6 AND 40
			  AND s.text !~ '[〜~…○□?？]' -- 句式占位符句子念出来不明所以，不出听力题
			UNION ALL
			SELECT '2ls', 'ls:' || s.id, s.text, COALESCE(s.ruby_tokens, '[]'::jsonb),
			       s.translation, a.id, '[]'::jsonb, ''::text
			FROM word_sentence ws
			JOIN sentence s ON s.id = ws.sentence_id
			LEFT JOIN LATERAL (
				SELECT aa.id FROM audio_asset aa
				WHERE aa.sentence_id = s.id AND aa.status = 'ready'
				ORDER BY (aa.rate = 1.0) DESC LIMIT 1
			) a ON true
			WHERE ws.word_id = $1
			  AND s.translation IS NOT NULL AND s.text LIKE '%' || $3 || '%'
			  AND char_length(s.text) BETWEEN 6 AND 40
			  AND s.text !~ '[〜~…○□?？]'
			UNION ALL
			SELECT '3wc', 'wc:' || c.segment_id, c.text_ja, '[]'::jsonb, c.text_zh,
			       NULL::text, COALESCE(c.tokens, '[]'::jsonb), c.audio_url
			FROM word w
			JOIN word_clip c ON c.word_key = word_clip_key(w.surface, w.reading)
			WHERE w.id = $1 AND w.user_id = $2
			  AND c.match_text = $3
			  AND c.zh_source = 'llm'
			  AND c.text_zh IS NOT NULL AND btrim(c.text_zh) <> ''
			  AND char_length(c.text_ja) BETWEEN 6 AND 40
			  AND c.text_ja !~ '[〜~…○□?？]'
			  AND c.audio_url <> ''
		) q ORDER BY src, sent_key LIMIT 12`, w.ID, userID, target)
	if err != nil {
		return nil
	}
	defer rows.Close()
	out := []sentRow{}
	seen := map[string]bool{}
	for rows.Next() {
		var r sentRow
		var ruby, clipTokens []byte
		if err := rows.Scan(&r.Src, &r.Key, &r.Text, &ruby, &r.Trans, &r.Audio,
			&clipTokens, &r.ClipAudio); err != nil {
			continue
		}
		r.Ruby = json.RawMessage(ruby)
		if r.Src == "3wc" {
			r.Ruby = clipRuby(json.RawMessage(clipTokens), r.Text)
			if r.Ruby == nil {
				continue
			}
		}
		if seen[r.Text] {
			continue
		}
		seen[r.Text] = true
		out = append(out, r)
	}
	return out
}

// lastSentence 上次答完时记录的例句 key；没记录返回空串。
func lastSentence(ctx context.Context, pool *pgxpool.Pool, wordID string) string {
	var key string
	if err := pool.QueryRow(ctx,
		`SELECT last_sent FROM word_quiz_state WHERE word_id = $1`, wordID).Scan(&key); err != nil {
		return ""
	}
	return key
}

// distractors 随机取 n 个互异干扰项（exclude 之外、非空）。
func distractors(ctx context.Context, pool *pgxpool.Pool, userID, col, exclude string, n int) []string {
	if col != "meaning" && col != "surface" {
		return nil
	}
	rows, err := pool.Query(ctx, `
		SELECT `+col+` FROM word
		WHERE user_id = $1 AND `+col+` IS NOT NULL AND `+col+` <> $2 AND `+col+` <> ''
		ORDER BY random() LIMIT $3`, userID, exclude, n)
	if err != nil {
		return nil
	}
	defer rows.Close()
	out := []string{}
	seen := map[string]bool{exclude: true}
	for rows.Next() {
		var v string
		if err := rows.Scan(&v); err != nil || seen[v] {
			continue
		}
		seen[v] = true
		out = append(out, v)
	}
	return out
}

// buildQuiz 按模式与序号轮换题型；素材不齐就降级，绝不硬凑坏卡。
// mix=true 用于到期复习/整目录测试：标准模式一场内交错四种题型；
// 新词首学传 false，保持认词/听音两种温和题型。
func buildQuiz(ctx context.Context, pool *pgxpool.Pool, userID string, w word, idx int, mode Mode, mix bool) *Quiz {
	if mode == ModeListening {
		if q := buildListening(ctx, pool, userID, w, idx); q != nil {
			return q
		}
	}
	return buildStandard(ctx, pool, userID, w, idx, mix)
}

func buildStandard(ctx context.Context, pool *pgxpool.Pool, userID string, w word, idx int, mix bool) *Quiz {
	want := qPickMean
	if mix {
		want = mixType(idx)
		if want == qClozePick || want == qClozeType {
			if q := buildCloze(ctx, pool, userID, w, want == qClozePick); q != nil {
				return q
			}
			// 没句可挖：按奇偶退到认词/听音，保证每词都有题
			want = qPickMean
			if idx%2 == 1 && !patternWord(w.Surface) {
				want = qHearMean
			}
		}
	} else if idx%2 == 1 && !patternWord(w.Surface) {
		want = qHearMean
	}
	return buildMeaning(ctx, pool, userID, w, want)
}

// mixType 一场测试内的交错顺序：看词选义 → 挖空选词 → 听词选义 → 挖空输入。
func mixType(idx int) quizType {
	return []quizType{qPickMean, qClozePick, qHearMean, qClozeType}[idx%4]
}

func buildMeaning(ctx context.Context, pool *pgxpool.Pool, userID string, w word, want quizType) *Quiz {
	meaning := ""
	if w.Meaning != nil {
		meaning = *w.Meaning
	}
	if meaning == "" {
		return &Quiz{Type: qFlip, Hint: nil}
	}
	// 句式条目（〜さん之类）念出来不明所以：只看不听
	if want == qHearMean && patternWord(w.Surface) {
		want = qPickMean
	}
	ds := distractors(ctx, pool, userID, "meaning", meaning, 3)
	if len(ds) < 3 {
		return &Quiz{Type: qFlip, Hint: nil}
	}
	opts := append(ds, meaning)
	rand.Shuffle(len(opts), func(i, j int) { opts[i], opts[j] = opts[j], opts[i] })
	at := 0
	for i, o := range opts {
		if o == meaning {
			at = i
		}
	}
	return &Quiz{Type: want, Options: opts, Answer: at, Hint: nil}
}

// buildListening 听力模式：句式条目挖"句式部分"（〜さんのおかげです → さんのおかげです）；
// 占位符原句已被 SQL 排除，只有补出来的具体句会被选中；没句返回 nil 由调用方退标准通道。
func buildListening(ctx context.Context, pool *pgxpool.Pool, userID string, w word, idx int) *Quiz {
	target := clozeTarget(w)
	if target == "" {
		return nil
	}
	return buildCloze(ctx, pool, userID, w, idx%2 == 0)
}

func clozeTarget(w word) string {
	if patternWord(w.Surface) {
		return patternTail(w.Surface)
	}
	return w.Surface
}

// buildCloze 选一句能准确挖空的例句。先按可信度选出最高质量档，
// 同档内若有多句，轮换避开上次用过的那句；质量绝不为了换句而降级。
func buildCloze(ctx context.Context, pool *pgxpool.Pool, userID string, w word, wantPick bool) *Quiz {
	target := clozeTarget(w)
	if target == "" {
		return nil
	}
	type cand struct {
		row    sentRow
		tokens []sentToken
	}
	best := []cand{}
	bestScore := scoreBad
	for _, sr := range pickSentences(ctx, pool, userID, w, target) {
		tokens, sc := blankTokensScored(sr.Ruby, sr.Text, target, wordReading(w))
		if tokens == nil {
			continue
		}
		if sc > bestScore {
			bestScore = sc
			best = best[:0]
		}
		if sc == bestScore {
			best = append(best, cand{sr, tokens})
		}
	}
	if len(best) == 0 {
		return nil
	}
	pick := best[0]
	if len(best) > 1 {
		if last := lastSentence(ctx, pool, w.ID); last != "" {
			for i := range best {
				if best[i].row.Key == last {
					pick = best[(i+1)%len(best)]
					break
				}
			}
		}
	}
	sr, tokens := pick.row, pick.tokens
	q := &Quiz{Tokens: tokens, Hint: blurHint(sr.Trans, w.Meaning), Trans: sr.Trans, SentKey: sr.Key}
	q.BlankText = target
	q.Accept = []string{w.Surface}
	if w.Reading != nil && *w.Reading != w.Surface {
		q.Accept = append(q.Accept, *w.Reading)
	}
	if target != w.Surface {
		q.Accept = append(q.Accept, target)
	}
	switch {
	case sr.Audio != nil:
		q.Audio = &sentAudio{Kind: "asset", ID: *sr.Audio}
	case sr.ClipAudio != "":
		seg := strings.TrimPrefix(sr.Key, "wc:")
		q.Audio = &sentAudio{
			Kind: "url",
			URL:  "/clipmedia/audio/" + url.PathEscape(seg),
			Alt:  sr.ClipAudio,
		}
	default:
		q.Audio = &sentAudio{Kind: "say", Text: sr.Text, Ruby: sr.Ruby}
	}
	if !wantPick {
		q.Type = qClozeType
		return q
	}
	ds := distractors(ctx, pool, userID, "surface", w.Surface, 3)
	if len(ds) < 3 {
		q.Type = qClozeType
		return q
	}
	opts := append(ds, w.Surface)
	rand.Shuffle(len(opts), func(i, j int) { opts[i], opts[j] = opts[j], opts[i] })
	at := 0
	for i, o := range opts {
		if o == w.Surface {
			at = i
		}
	}
	q.Type = qClozePick
	q.Options = opts
	q.Answer = at
	return q
}
