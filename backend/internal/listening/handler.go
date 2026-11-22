// Package listening 听力训练：真人听力素材按大题播放，作答判断对错/选择题，
// 判分后揭晓原文与译文。内容全局共享，进度按 (user, lesson, section) 存。
package listening

import (
	"encoding/json"
	"net/http"
	"strconv"
	"strings"

	"github.com/go-chi/chi/v5"
	"github.com/greatgeek/TingShuo/backend/internal/auth"
	"github.com/greatgeek/TingShuo/backend/internal/httpx"
	"github.com/jackc/pgx/v5/pgxpool"
)

type Handler struct {
	pool *pgxpool.Pool
}

func NewHandler(pool *pgxpool.Pool) *Handler { return &Handler{pool: pool} }

type lessonItem struct {
	ID           string `json:"id"`
	LessonNo     int    `json:"lesson_no"`
	Title        string `json:"title"`
	DurationMS   int    `json:"duration_ms"`
	SectionCount int    `json:"section_count"`
	DoneCount    int    `json:"done_count"`
	Score        int    `json:"score"`
	Total        int    `json:"total"`
}

// List GET /listening/lessons：听力课列表（含当前用户各课已答大题数与总分）。
func (h *Handler) List(w http.ResponseWriter, r *http.Request) {
	userID := auth.FromContext(r.Context())
	rows, err := h.pool.Query(r.Context(), `
		SELECT l.id, l.lesson_no, l.title, l.duration_ms,
		       (SELECT count(*) FROM listening_section s WHERE s.lesson_id = l.id),
		       COALESCE(p.done, 0), COALESCE(p.score, 0), COALESCE(p.total, 0)
		FROM listening_lesson l
		LEFT JOIN (
			SELECT lesson_id, count(*) AS done, sum(score) AS score, sum(total) AS total
			FROM listening_progress WHERE user_id = $1
			GROUP BY lesson_id
		) p ON p.lesson_id = l.id
		ORDER BY l.sort_order, l.lesson_no`, userID)
	if err != nil {
		httpx.Err(w, httpx.Internal(err.Error()))
		return
	}
	defer rows.Close()
	items := []lessonItem{}
	for rows.Next() {
		var it lessonItem
		if err := rows.Scan(&it.ID, &it.LessonNo, &it.Title, &it.DurationMS,
			&it.SectionCount, &it.DoneCount, &it.Score, &it.Total); err != nil {
			httpx.Err(w, httpx.Internal(err.Error()))
			return
		}
		items = append(items, it)
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"items": items, "count": len(items)})
}

type questionItem struct {
	QuestionNo int             `json:"question_no"`
	Prompt     string          `json:"prompt"`
	Options    json.RawMessage `json:"options"`
	Answer     string          `json:"answer"`
}

type lineItem struct {
	ID          string          `json:"id"`
	LineNo      int             `json:"line_no"`
	Speaker     string          `json:"speaker"`
	StartMS     int             `json:"start_ms"`
	EndMS       int             `json:"end_ms"`
	Text        string          `json:"text"`
	Translation string          `json:"translation"`
	RubyTokens  json.RawMessage `json:"ruby_tokens"`
}

type progressItem struct {
	Answers map[string]string `json:"answers"`
	Score   int               `json:"score"`
	Total   int               `json:"total"`
}

type sectionItem struct {
	ID           string         `json:"id"`
	SectionNo    int            `json:"section_no"`
	Kind         string         `json:"kind"`
	Instruction  string         `json:"instruction"`
	AudioStartMS int            `json:"audio_start_ms"`
	AudioEndMS   int            `json:"audio_end_ms"`
	Example      *exampleItem   `json:"example,omitempty"`
	Questions    []questionItem `json:"questions"`
	Lines        []lineItem     `json:"lines"`
	Progress     *progressItem  `json:"progress"`
}

type exampleItem struct {
	Prompt string `json:"prompt"`
	Answer string `json:"answer"`
}

// Detail GET /listening/lessons/{id}：听力课结构（大题/题目/原文时间轴/当前用户作答）。
// 正确答案只在该大题已提交过之后返回，避免没作答就拿到答案。
func (h *Handler) Detail(w http.ResponseWriter, r *http.Request) {
	userID := auth.FromContext(r.Context())
	lessonID := chi.URLParam(r, "id")
	var lesson lessonItem
	var audioKey string
	if err := h.pool.QueryRow(r.Context(),
		`SELECT id, lesson_no, title, duration_ms, audio_key FROM listening_lesson WHERE id = $1`,
		lessonID).Scan(&lesson.ID, &lesson.LessonNo, &lesson.Title, &lesson.DurationMS, &audioKey); err != nil {
		httpx.Err(w, httpx.NotFound("听力课不存在"))
		return
	}
	audioURL := audioKey
	if !strings.HasPrefix(audioKey, "http") {
		audioURL = "/media/" + strings.TrimPrefix(audioKey, "/")
	}

	rows, err := h.pool.Query(r.Context(), `
		SELECT s.id, s.section_no, s.kind, s.instruction, s.audio_start_ms, s.audio_end_ms,
		       s.example_prompt, s.example_answer,
		       p.answers, p.score, p.total
		FROM listening_section s
		LEFT JOIN listening_progress p
		  ON p.lesson_id = s.lesson_id AND p.section_no = s.section_no AND p.user_id = $2
		WHERE s.lesson_id = $1
		ORDER BY s.section_no`, lessonID, userID)
	if err != nil {
		httpx.Err(w, httpx.Internal(err.Error()))
		return
	}
	sections := []*sectionItem{}
	for rows.Next() {
		var s sectionItem
		var examplePrompt, exampleAnswer string
		var answers []byte
		var score, total *int
		if err := rows.Scan(&s.ID, &s.SectionNo, &s.Kind, &s.Instruction,
			&s.AudioStartMS, &s.AudioEndMS, &examplePrompt, &exampleAnswer,
			&answers, &score, &total); err != nil {
			httpx.Err(w, httpx.Internal(err.Error()))
			return
		}
		if examplePrompt != "" {
			s.Example = &exampleItem{Prompt: examplePrompt, Answer: exampleAnswer}
		}
		s.Questions = []questionItem{}
		s.Lines = []lineItem{}
		if score != nil && total != nil {
			pr := &progressItem{Answers: map[string]string{}, Score: *score, Total: *total}
			if len(answers) > 0 {
				_ = json.Unmarshal(answers, &pr.Answers)
			}
			s.Progress = pr
		}
		sections = append(sections, &s)
	}
	rows.Close()
	if len(sections) == 0 {
		httpx.WriteJSON(w, http.StatusOK, map[string]any{"lesson": lesson, "audio_url": audioURL, "sections": []any{}})
		return
	}

	qRows, err := h.pool.Query(r.Context(), `
		SELECT q.section_id, q.question_no, q.prompt, q.options,
		       CASE WHEN p.user_id IS NOT NULL THEN q.answer ELSE '' END
		FROM listening_question q
		JOIN listening_section s ON s.id = q.section_id
		LEFT JOIN listening_progress p
		  ON p.lesson_id = s.lesson_id AND p.section_no = s.section_no AND p.user_id = $2
		WHERE s.lesson_id = $1
		ORDER BY s.section_no, q.question_no`, lessonID, userID)
	if err != nil {
		httpx.Err(w, httpx.Internal(err.Error()))
		return
	}
	sectionByID := map[string]*sectionItem{}
	for _, s := range sections {
		sectionByID[s.ID] = s
	}
	for qRows.Next() {
		var sectionID string
		var q questionItem
		var options []byte
		if err := qRows.Scan(&sectionID, &q.QuestionNo, &q.Prompt, &options, &q.Answer); err != nil {
			httpx.Err(w, httpx.Internal(err.Error()))
			return
		}
		if len(options) > 0 {
			q.Options = json.RawMessage(options)
		}
		if s := sectionByID[sectionID]; s != nil {
			s.Questions = append(s.Questions, q)
		}
	}
	qRows.Close()

	lRows, err := h.pool.Query(r.Context(), `
		SELECT l.section_id, l.id, l.line_no, l.speaker, l.start_ms, l.end_ms,
		       l.text, l.translation, l.ruby_tokens
		FROM listening_line l
		JOIN listening_section s ON s.id = l.section_id
		WHERE s.lesson_id = $1
		ORDER BY s.section_no, l.line_no`, lessonID)
	if err != nil {
		httpx.Err(w, httpx.Internal(err.Error()))
		return
	}
	defer lRows.Close()
	for lRows.Next() {
		var sectionID string
		var ln lineItem
		var ruby []byte
		if err := lRows.Scan(&sectionID, &ln.ID, &ln.LineNo, &ln.Speaker, &ln.StartMS, &ln.EndMS,
			&ln.Text, &ln.Translation, &ruby); err != nil {
			httpx.Err(w, httpx.Internal(err.Error()))
			return
		}
		if len(ruby) > 0 {
			ln.RubyTokens = json.RawMessage(ruby)
		}
		if s := sectionByID[sectionID]; s != nil {
			s.Lines = append(s.Lines, ln)
		}
	}
	lesson.SectionCount = len(sections)
	httpx.WriteJSON(w, http.StatusOK, map[string]any{
		"lesson": lesson, "audio_url": audioURL, "sections": sections,
	})
}

type submitReq struct {
	Answers map[string]string `json:"answers"`
}

type questionResult struct {
	QuestionNo int    `json:"question_no"`
	Correct    bool   `json:"correct"`
	Answer     string `json:"answer"`
	Your       string `json:"your"`
}

// Submit POST /listening/lessons/{id}/sections/{no}/submit：判分并记录该大题作答。
func (h *Handler) Submit(w http.ResponseWriter, r *http.Request) {
	userID := auth.FromContext(r.Context())
	lessonID := chi.URLParam(r, "id")
	sectionNo, err := strconv.Atoi(chi.URLParam(r, "no"))
	if err != nil {
		httpx.Err(w, httpx.BadRequest("BAD_SECTION", "大题编号不合法"))
		return
	}
	var req submitReq
	if e := httpx.Decode(r, &req); e != nil {
		httpx.Err(w, e)
		return
	}
	var userOK bool
	if err := h.pool.QueryRow(r.Context(),
		`SELECT EXISTS(SELECT 1 FROM "user" WHERE id = $1 AND deleted_at IS NULL)`, userID).Scan(&userOK); err != nil {
		httpx.Err(w, httpx.Internal(err.Error()))
		return
	}
	if !userOK {
		httpx.Err(w, httpx.Unauthorized("登录状态已失效，请重新登录"))
		return
	}
	rows, err := h.pool.Query(r.Context(), `
		SELECT q.question_no, q.answer
		FROM listening_question q
		JOIN listening_section s ON s.id = q.section_id
		WHERE s.lesson_id = $1 AND s.section_no = $2
		ORDER BY q.question_no`, lessonID, sectionNo)
	if err != nil {
		httpx.Err(w, httpx.Internal(err.Error()))
		return
	}
	var keys []int
	var answers []string
	for rows.Next() {
		var no int
		var ans string
		if err := rows.Scan(&no, &ans); err != nil {
			rows.Close()
			httpx.Err(w, httpx.Internal(err.Error()))
			return
		}
		keys = append(keys, no)
		answers = append(answers, ans)
	}
	rows.Close()
	if len(keys) == 0 {
		httpx.Err(w, httpx.NotFound("大题不存在"))
		return
	}

	normalized := map[string]string{}
	score := 0
	results := make([]questionResult, 0, len(keys))
	for i, no := range keys {
		k := strconv.Itoa(no)
		your := strings.TrimSpace(req.Answers[k])
		ok := answerOK(answers[i], your)
		if ok {
			score++
		}
		normalized[k] = your
		results = append(results, questionResult{QuestionNo: no, Correct: ok, Answer: answers[i], Your: your})
	}
	b, _ := json.Marshal(normalized)
	if _, err := h.pool.Exec(r.Context(), `
		INSERT INTO listening_progress (user_id, lesson_id, section_no, answers, score, total)
		VALUES ($1, $2, $3, $4, $5, $6)
		ON CONFLICT (user_id, lesson_id, section_no)
		DO UPDATE SET answers = EXCLUDED.answers, score = EXCLUDED.score,
		              total = EXCLUDED.total, updated_at = now()`,
		userID, lessonID, sectionNo, b, score, len(keys)); err != nil {
		httpx.Err(w, httpx.Internal(err.Error()))
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{
		"score": score, "total": len(keys), "results": results,
	})
}