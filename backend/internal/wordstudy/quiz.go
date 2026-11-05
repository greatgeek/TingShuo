package wordstudy

// 背词 quiz 载荷：模式开关决定"这次用哪条通道测"，单词仍是调度单元。
// 标准模式轮换 看词选义/听词选义；听力模式轮换 听句挖空选词/听句挖空输入。
// 挖空卡正面只给语境不给答案：目标词位置空白且不带振假名（注音即答案），
// 语境提示=句子翻译里把词义替换成○○的模糊版，替换不了就不给提示（宁缺勿泄）。

import (
	"encoding/json"
	"math/rand"
	"strings"

	"context"

	"github.com/jackc/pgx/v5/pgxpool"
)

type quizType string

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
	Kind string `json:"kind"` // asset=audio_asset 点播；say=/audio/say 文本合成缓存
	ID   string `json:"id,omitempty"`
	Text string `json:"text,omitempty"`
}

type sentToken struct {
	Text    string  `json:"text"`
	Reading *string `json:"reading"`
	Blank   bool    `json:"blank,omitempty"`
}

type Quiz struct {
	Type    quizType  `json:"type"`
	Options []string  `json:"options,omitempty"` // pick_mean/hear_mean=释义；cloze_pick=词面
	Answer  int       `json:"answer"`            // Options 下标
	Tokens  []sentToken `json:"tokens,omitempty"` // 挖空句逐 token（空白 token 无读音）
	Hint    *string   `json:"hint"`               // 模糊化句子翻译；nil=本卡不提供提示
	Trans   *string   `json:"trans,omitempty"`    // 背面第二层：完整句子翻译
	Audio   *sentAudio `json:"audio,omitempty"`
}

// blankTokens 把词面在句中的位置换成单个空白 token 且剥掉读音。
// 与空白区间有任何重叠的 token 都并进空白，避免半个词漏在外面提示答案。
func blankTokens(raw json.RawMessage, text, surface string) []sentToken {
	// ruby_tokens 落库形态是 [[text, reading|null], ...] 的二元组数组
	var src [][2]*string
	if err := json.Unmarshal(raw, &src); err != nil || len(src) == 0 {
		return nil
	}
	start := strings.Index(text, surface)
	if start < 0 {
		return nil
	}
	end := start + len(surface)
	out := []sentToken{}
	off := 0
	blanked := false
	for _, p := range src {
		if p[0] == nil {
			continue
		}
		tStart, tEnd := off, off+len(*p[0])
		off = tEnd
		if tStart < end && tEnd > start { // 与空白区间重叠
			if !blanked {
				out = append(out, sentToken{Text: "＿＿＿", Blank: true})
				blanked = true
			}
			continue
		}
		out = append(out, sentToken{Text: *p[0], Reading: p[1]})
	}
	if !blanked {
		return nil
	}
	return out
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
	Src   string
	Text  string
	Ruby  json.RawMessage
	Trans *string
	Audio *string
}

// pickSentence 挖空素材：生词本句优先（有翻译、导入质检过），回退课程句。
func pickSentence(ctx context.Context, pool *pgxpool.Pool, userID, wordID, surface string) (*sentRow, error) {
	var r sentRow
	var ruby []byte
	err := pool.QueryRow(ctx, `
		SELECT src, text, ruby_tokens, translation, audio_id FROM (
			SELECT '1nb' AS src, s.text, s.ruby_tokens, s.translation, NULL::text AS audio_id
			FROM notebook_sentence s
			JOIN notebook_word nw ON nw.id = s.notebook_word_id
			JOIN notebook nb     ON nb.id = nw.notebook_id
			WHERE nw.word_id = $1 AND nb.user_id = $2
			  AND s.translation IS NOT NULL AND s.text LIKE '%' || $3 || '%'
			  AND char_length(s.text) BETWEEN 6 AND 40
			UNION ALL
			SELECT '2ls', s.text, s.ruby_tokens, s.translation, a.id
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
		) q ORDER BY src LIMIT 1`, wordID, userID, surface).
		Scan(&r.Src, &r.Text, &ruby, &r.Trans, &r.Audio)
	if err != nil {
		return nil, err
	}
	r.Ruby = json.RawMessage(ruby)
	return &r, nil
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
func buildQuiz(ctx context.Context, pool *pgxpool.Pool, userID string, w word, idx int, mode Mode) *Quiz {
	shuffle := func(opts []string, answer string) ([]string, int) {
		rand.Shuffle(len(opts), func(i, j int) { opts[i], opts[j] = opts[j], opts[i] })
		at := 0
		for i, o := range opts {
			if o == answer {
				at = i
			}
		}
		return opts, at
	}

	meaning := ""
	if w.Meaning != nil {
		meaning = *w.Meaning
	}

	if mode == ModeStandard {
		want := qPickMean
		if idx%2 == 1 {
			want = qHearMean
		}
		if meaning == "" {
			return &Quiz{Type: qFlip, Hint: nil}
		}
		ds := distractors(ctx, pool, userID, "meaning", meaning, 3)
		if len(ds) < 3 {
			return &Quiz{Type: qFlip, Hint: nil}
		}
		opts, at := shuffle(append(ds, meaning), meaning)
		return &Quiz{Type: want, Options: opts, Answer: at, Hint: nil}
	}

	// 听力模式：先找句，没句就退标准通道
	sr, err := pickSentence(ctx, pool, userID, w.ID, w.Surface)
	if err != nil || sr == nil {
		return buildQuiz(ctx, pool, userID, w, idx, ModeStandard)
	}
	tokens := blankTokens(sr.Ruby, sr.Text, w.Surface)
	if tokens == nil {
		return buildQuiz(ctx, pool, userID, w, idx, ModeStandard)
	}
	q := &Quiz{Tokens: tokens, Hint: blurHint(sr.Trans, w.Meaning), Trans: sr.Trans}
	if sr.Audio != nil {
		q.Audio = &sentAudio{Kind: "asset", ID: *sr.Audio}
	} else {
		q.Audio = &sentAudio{Kind: "say", Text: sr.Text}
	}
	if idx%2 == 1 {
		q.Type = qClozeType
		return q
	}
	ds := distractors(ctx, pool, userID, "surface", w.Surface, 3)
	if len(ds) < 3 {
		q.Type = qClozeType
		return q
	}
	opts, at := shuffle(append(ds, w.Surface), w.Surface)
	q.Type = qClozePick
	q.Options = opts
	q.Answer = at
	return q
}
