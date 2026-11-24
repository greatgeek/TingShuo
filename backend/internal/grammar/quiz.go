package grammar

import (
	"encoding/json"
	"math/rand"
	"net/http"
	"strconv"
	"strings"

	"github.com/greatgeek/TingShuo/backend/internal/auth"
	"github.com/greatgeek/TingShuo/backend/internal/httpx"
	"github.com/jackc/pgx/v5"
)

// 语法测验：按课取题（每个知识点随机 1 题）或整册随机抽题。
// 题库选项按固定顺序存库，下发时乱序并给出正确答案下标，前端点选即判。

type quizOption struct {
	Text  string     `json:"text"`
	Ruby  [][]string `json:"ruby,omitempty"`
	Why   string     `json:"why,omitempty"`
}

type storedOption struct {
	Text    string     `json:"text"`
	Ruby    [][]string `json:"ruby"`
	Correct bool       `json:"correct"`
	Why     string     `json:"why"`
}

type quizSource struct {
	Text        string          `json:"text"`
	Translation string          `json:"translation"`
	Ruby        json.RawMessage `json:"ruby_tokens"`
}

type quizQuestion struct {
	ID        string          `json:"id"`
	PointID   string          `json:"point_id"`
	Pattern   string          `json:"point_pattern"`
	Formation string          `json:"formation,omitempty"`
	Section   string          `json:"section"`
	Book      string          `json:"book"`
	LessonNo  int             `json:"lesson_no"`
	Kind      string          `json:"kind"`
	Stem      string          `json:"stem"`
	StemRuby  json.RawMessage `json:"stem_ruby"`
	Options   []quizOption    `json:"options"`
	Answer    int             `json:"answer"`
	Explain   string          `json:"explain"`
	Source    *quizSource     `json:"source,omitempty"`
}

const quizColumns = `
	q.id, q.kind, q.stem, q.stem_ruby, q.options, q.explain,
	gp.id, gp.pattern, gp.formation, gp.section, gp.book, gp.lesson_no,
	ge.text, ge.translation, COALESCE(ge.ruby_tokens, '[]'::jsonb)`

// quizSets 专属套题：tag → 标题。只在显式传 set 时抽取，普通课测/整册测验排除。
var quizSets = map[string]string{
	"adj-i":       "Ⅰ类形容词 · 现在・将来",
	"adj-na":      "Ⅱ类形容词 · 现在・将来",
	"adj-past-i":  "Ⅰ类形容词 · 现在・过去",
	"adj-past-na": "Ⅱ类形容词 · 现在・过去",
}

// Quiz GET /grammar/quiz?book=&lesson=|n=
// lesson>0：本课每个知识点随机抽 1 题；否则整册随机 n 题（默认 20，上限 40）。
func (h *Handler) Quiz(w http.ResponseWriter, r *http.Request) {
	book := bookParam(r.URL.Query().Get("book"))
	lesson, _ := strconv.Atoi(r.URL.Query().Get("lesson"))
	n, _ := strconv.Atoi(r.URL.Query().Get("n"))
	set := strings.TrimSpace(r.URL.Query().Get("set"))
	if n <= 0 {
		n = 20
	}
	if n > 40 {
		n = 40
	}

	title := "综合测验"
	var rows pgx.Rows
	var err error
	if set != "" {
		var ok bool
		title, ok = quizSets[set]
		if !ok {
			httpx.Err(w, httpx.BadRequest("QUIZ_SET_INVALID", "没有这套题"))
			return
		}
		lesson = 0
		rows, err = h.pool.Query(r.Context(), `
			SELECT `+quizColumns+`
			FROM grammar_question q
			JOIN grammar_point gp ON gp.id = q.point_id
			LEFT JOIN grammar_example ge ON ge.id = q.source_example_id
			WHERE q.status = 'ok' AND q.tag = $1
			ORDER BY random()
			LIMIT 200`, set)
	} else if lesson > 0 {
		if e := h.pool.QueryRow(r.Context(),
			`SELECT title FROM grammar_lesson WHERE book = $1 AND lesson_no = $2`, book, lesson).Scan(&title); e != nil {
			httpx.Err(w, httpx.NotFound("没有这课的语法内容"))
			return
		}
		rows, err = h.pool.Query(r.Context(), `
			SELECT `+quizColumns+`
			FROM (
				SELECT DISTINCT ON (point_id) id, point_id, kind, stem, stem_ruby, options, explain, source_example_id
				FROM grammar_question
				WHERE status = 'ok'
				  AND tag = ''
				  AND point_id IN (SELECT id FROM grammar_point WHERE book = $1 AND lesson_no = $2)
				ORDER BY point_id, random()
			) q
			JOIN grammar_point gp ON gp.id = q.point_id
			LEFT JOIN grammar_example ge ON ge.id = q.source_example_id
			ORDER BY gp.seq`, book, lesson)
	} else {
		rows, err = h.pool.Query(r.Context(), `
			SELECT `+quizColumns+`
			FROM (
				SELECT DISTINCT ON (point_id) id, point_id, kind, stem, stem_ruby, options, explain,
				       source_example_id, random() AS rnd
				FROM grammar_question q
				WHERE status = 'ok'
				  AND q.tag = ''
				  AND point_id IN (SELECT id FROM grammar_point WHERE book = $1)
				ORDER BY point_id, random()
			) q
			JOIN grammar_point gp ON gp.id = q.point_id
			LEFT JOIN grammar_example ge ON ge.id = q.source_example_id
			ORDER BY q.rnd
			LIMIT $2`, book, n)
	}
	if err != nil {
		httpx.Err(w, httpx.Internal(err.Error()))
		return
	}
	defer rows.Close()

	questions := []quizQuestion{}
	for rows.Next() {
		var q quizQuestion
		var stemRuby, optionsRaw []byte
		var srcText, srcTrans *string
		var srcRubyRaw []byte
		if err := rows.Scan(
			&q.ID, &q.Kind, &q.Stem, &stemRuby, &optionsRaw, &q.Explain,
			&q.PointID, &q.Pattern, &q.Formation, &q.Section, &q.Book, &q.LessonNo,
			&srcText, &srcTrans, &srcRubyRaw,
		); err != nil {
			httpx.Err(w, httpx.Internal(err.Error()))
			return
		}
		q.StemRuby = json.RawMessage(stemRuby)
		if len(q.StemRuby) == 0 {
			q.StemRuby = json.RawMessage(`[]`)
		}
		var stored []storedOption
		if err := json.Unmarshal(optionsRaw, &stored); err != nil {
			continue
		}
		options, answer := presentOptions(stored)
		if len(options) != 4 || answer < 0 {
			continue
		}
		q.Options, q.Answer = options, answer
		if srcText != nil && strings.TrimSpace(*srcText) != "" {
			q.Source = &quizSource{Text: *srcText, Ruby: json.RawMessage(srcRubyRaw)}
			if srcTrans != nil {
				q.Source.Translation = *srcTrans
			}
			if len(q.Source.Ruby) == 0 {
				q.Source.Ruby = json.RawMessage(`[]`)
			}
		}
		questions = append(questions, q)
	}
	if err := rows.Err(); err != nil {
		httpx.Err(w, httpx.Internal(err.Error()))
		return
	}

	httpx.WriteJSON(w, http.StatusOK, map[string]any{
		"book":      book,
		"lesson_no": lesson,
		"title":     title,
		"questions": questions,
	})
}

// presentOptions 乱序选项并定位正确答案（存库顺序固定，每次做题顺序不同）。
func presentOptions(stored []storedOption) ([]quizOption, int) {
	idx := make([]int, len(stored))
	answer := -1
	for i, o := range stored {
		idx[i] = i
		if o.Correct {
			if answer >= 0 {
				return nil, -1 // 多个正确答案视为坏题，跳过
			}
			answer = i
		}
	}
	if answer < 0 {
		return nil, -1
	}
	rand.Shuffle(len(idx), func(i, j int) { idx[i], idx[j] = idx[j], idx[i] })
	out := make([]quizOption, len(stored))
	newAnswer := 0
	for i, k := range idx {
		out[i] = quizOption{Text: stored[k].Text, Ruby: stored[k].Ruby, Why: stored[k].Why}
		if k == answer {
			newAnswer = i
		}
	}
	return out, newAnswer
}

// Attempt POST /grammar/quiz/attempt：记录作答（对错以库内答案为准）。
func (h *Handler) Attempt(w http.ResponseWriter, r *http.Request) {
	userID := auth.FromContext(r.Context())
	var req struct {
		QuestionID string `json:"question_id"`
		ChosenText string `json:"chosen_text"`
	}
	if e := httpx.Decode(r, &req); e != nil {
		httpx.Err(w, e)
		return
	}
	req.QuestionID = strings.TrimSpace(req.QuestionID)
	req.ChosenText = strings.TrimSpace(req.ChosenText)
	if req.QuestionID == "" || req.ChosenText == "" {
		httpx.Err(w, httpx.BadRequest("BAD_ATTEMPT", "题目参数无效"))
		return
	}
	var optionsRaw []byte
	if err := h.pool.QueryRow(r.Context(),
		`SELECT options FROM grammar_question WHERE id = $1 AND status = 'ok'`, req.QuestionID).Scan(&optionsRaw); err != nil {
		httpx.Err(w, httpx.NotFound("题目不存在"))
		return
	}
	var stored []storedOption
	if err := json.Unmarshal(optionsRaw, &stored); err != nil {
		httpx.Err(w, httpx.BadRequest("BAD_ATTEMPT", "选项无效"))
		return
	}
	// 客户端拿到的是乱序选项，按文本回查存库下标（同一题内选项不重复）
	chosen, correct := -1, false
	for i, o := range stored {
		if o.Text == req.ChosenText {
			chosen, correct = i, o.Correct
			break
		}
	}
	if chosen < 0 {
		httpx.Err(w, httpx.BadRequest("BAD_ATTEMPT", "选项无效"))
		return
	}
	if _, err := h.pool.Exec(r.Context(), `
		INSERT INTO grammar_attempt (user_id, question_id, chosen, correct)
		VALUES ($1, $2, $3, $4)`, userID, req.QuestionID, chosen, correct); err != nil {
		httpx.Err(w, httpx.Internal(err.Error()))
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"ok": true, "correct": correct})
}
