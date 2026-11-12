// Package grammar 语法本：按教材课次的全局语法内容 + 当前用户的例句联动匹配。
// 联动不落库：例句与生词本例句、课文本原文按归一化文本实时精确匹配。
package grammar

import (
	"encoding/json"
	"net/http"
	"regexp"
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

func bookParam(v string) string {
	if v = strings.TrimSpace(v); v != "" {
		return v
	}
	return "初级上"
}

type lessonItem struct {
	LessonNo           int    `json:"lesson_no"`
	Title              string `json:"title"`
	Points             int    `json:"points"`
	GrammarPoints      int    `json:"grammar_points"`
	ExpressionPoints   int    `json:"expression_points"`
	Examples           int    `json:"examples"`
	RecitationLessonID string `json:"recitation_lesson_id,omitempty"`
	HasNotebook        bool   `json:"has_notebook"`
}

// Lessons GET /grammar/lessons?book=初级上：课列表（语法点数、是否有课文本/生词本）。
func (h *Handler) Lessons(w http.ResponseWriter, r *http.Request) {
	userID := auth.FromContext(r.Context())
	book := bookParam(r.URL.Query().Get("book"))
	rows, err := h.pool.Query(r.Context(), `
		SELECT gl.lesson_no, gl.title,
		       (SELECT count(*) FROM grammar_point gp
		         WHERE gp.book = gl.book AND gp.lesson_no = gl.lesson_no),
		       (SELECT count(*) FROM grammar_point gp
		         WHERE gp.book = gl.book AND gp.lesson_no = gl.lesson_no AND gp.section = 'grammar'),
		       (SELECT count(*) FROM grammar_point gp
		         WHERE gp.book = gl.book AND gp.lesson_no = gl.lesson_no AND gp.section = 'expression'),
		       (SELECT count(*) FROM grammar_example ge
		          JOIN grammar_point gp ON gp.id = ge.point_id
		         WHERE gp.book = gl.book AND gp.lesson_no = gl.lesson_no)
		FROM grammar_lesson gl
		WHERE gl.book = $1
		ORDER BY gl.sort_order, gl.lesson_no`, book)
	if err != nil {
		httpx.Err(w, httpx.Internal(err.Error()))
		return
	}
	items := []lessonItem{}
	index := map[int]int{}
	for rows.Next() {
		var it lessonItem
		if err := rows.Scan(&it.LessonNo, &it.Title, &it.Points, &it.GrammarPoints, &it.ExpressionPoints, &it.Examples); err != nil {
			rows.Close()
			httpx.Err(w, httpx.Internal(err.Error()))
			return
		}
		index[it.LessonNo] = len(items)
		items = append(items, it)
	}
	rows.Close()

	recRows, err := h.pool.Query(r.Context(),
		`SELECT lesson_no, id FROM recitation_lesson ORDER BY sort_order, lesson_no`)
	if err == nil {
		for recRows.Next() {
			var no int
			var id string
			if recRows.Scan(&no, &id) == nil {
				if i, ok := index[no]; ok {
					items[i].RecitationLessonID = id
				}
			}
		}
		recRows.Close()
	}

	nbRows, err := h.pool.Query(r.Context(), `
		SELECT DISTINCT d.name
		FROM notebook_dir d
		JOIN notebook nb ON nb.id = d.notebook_id
		WHERE nb.user_id = $1`, userID)
	if err == nil {
		re := regexp.MustCompile(`^第(\d+)课$`)
		for nbRows.Next() {
			var name string
			if nbRows.Scan(&name) == nil {
				if m := re.FindStringSubmatch(name); m != nil {
					if no, err := strconv.Atoi(m[1]); err == nil {
						if i, ok := index[no]; ok {
							items[i].HasNotebook = true
						}
					}
				}
			}
		}
		nbRows.Close()
	}

	httpx.WriteJSON(w, http.StatusOK, map[string]any{"book": book, "lessons": items})
}

type recRef struct {
	LessonID string `json:"lesson_id"`
	LineNo   int    `json:"line_no"`
}

type nbRef struct {
	NotebookID string `json:"notebook_id"`
	DirID      string `json:"dir_id"`
	WordID     string `json:"word_id"`
	Surface    string `json:"surface"`
}

type example struct {
	Seq         int             `json:"seq"`
	Speaker     string          `json:"speaker,omitempty"`
	Mark        string          `json:"mark,omitempty"`
	Text        string          `json:"text"`
	Translation string          `json:"translation"`
	Note        string          `json:"note,omitempty"`
	Ruby        json.RawMessage `json:"ruby_tokens"`
	Rec         *recRef         `json:"rec,omitempty"`
	Notebook    *nbRef          `json:"notebook,omitempty"`
}

type point struct {
	ID        string          `json:"id"`
	Seq       int             `json:"seq"`
	Section   string          `json:"section"`
	Pattern   string          `json:"pattern"`
	Formation string          `json:"formation"`
	Body      json.RawMessage `json:"body"`
	Examples  []example       `json:"examples"`
}

// Lesson GET /grammar/lessons/{no}?book=初级上：一课语法点+例句，例句带当前用户的联动命中。
func (h *Handler) Lesson(w http.ResponseWriter, r *http.Request) {
	userID := auth.FromContext(r.Context())
	book := bookParam(r.URL.Query().Get("book"))
	no, err := strconv.Atoi(chi.URLParam(r, "no"))
	if err != nil || no <= 0 {
		httpx.Err(w, httpx.BadRequest("LESSON_INVALID", "课号无效"))
		return
	}
	var title string
	if err := h.pool.QueryRow(r.Context(),
		`SELECT title FROM grammar_lesson WHERE book = $1 AND lesson_no = $2`, book, no).Scan(&title); err != nil {
		httpx.Err(w, httpx.NotFound("没有这课的语法内容"))
		return
	}

	points := []point{}
	rows, err := h.pool.Query(r.Context(), `
		SELECT id, seq, section, pattern, formation, body
		FROM grammar_point WHERE book = $1 AND lesson_no = $2 ORDER BY seq`, book, no)
	if err != nil {
		httpx.Err(w, httpx.Internal(err.Error()))
		return
	}
	pointIndex := map[string]int{}
	for rows.Next() {
		var p point
		var body []byte
		if err := rows.Scan(&p.ID, &p.Seq, &p.Section, &p.Pattern, &p.Formation, &body); err != nil {
			rows.Close()
			httpx.Err(w, httpx.Internal(err.Error()))
			return
		}
		p.Body = json.RawMessage(body)
		p.Examples = []example{}
		pointIndex[p.ID] = len(points)
		points = append(points, p)
	}
	rows.Close()

	exRows, err := h.pool.Query(r.Context(), `
		SELECT ge.point_id, ge.seq, ge.speaker, ge.mark, ge.text, ge.translation, ge.note,
		       COALESCE(ge.ruby_tokens, '[]'::jsonb)
		FROM grammar_example ge
		JOIN grammar_point gp ON gp.id = ge.point_id
		WHERE gp.book = $1 AND gp.lesson_no = $2
		ORDER BY gp.seq, ge.seq`, book, no)
	if err != nil {
		httpx.Err(w, httpx.Internal(err.Error()))
		return
	}
	for exRows.Next() {
		var pid string
		var e example
		var ruby []byte
		if err := exRows.Scan(&pid, &e.Seq, &e.Speaker, &e.Mark, &e.Text, &e.Translation, &e.Note, &ruby); err != nil {
			exRows.Close()
			httpx.Err(w, httpx.Internal(err.Error()))
			return
		}
		e.Ruby = json.RawMessage(ruby)
		if i, ok := pointIndex[pid]; ok {
			points[i].Examples = append(points[i].Examples, e)
		}
	}
	exRows.Close()

	// 课文本：同课原文行（内容全局）
	var recID string
	_ = h.pool.QueryRow(r.Context(),
		`SELECT id FROM recitation_lesson WHERE lesson_no = $1 ORDER BY sort_order LIMIT 1`, no).Scan(&recID)
	recMatches := map[string]int{}
	if recID != "" {
		recRows, err := h.pool.Query(r.Context(),
			`SELECT line_no, text FROM recitation_line WHERE lesson_id = $1 ORDER BY line_no`, recID)
		if err == nil {
			for recRows.Next() {
				var lineNo int
				var text string
				if recRows.Scan(&lineNo, &text) == nil {
					if _, ok := recMatches[normText(text)]; !ok {
						recMatches[normText(text)] = lineNo
					}
				}
			}
			recRows.Close()
		}
	}

	// 生词本：当前用户「第N课」下所有例句（按归一化文本取首个命中）
	nbMatches := map[string]nbRef{}
	nbRows, err := h.pool.Query(r.Context(), `
		SELECT nb.id, nw.dir_id, w.id, w.surface, s.text
		FROM notebook_sentence s
		JOIN notebook_word nw ON nw.id = s.notebook_word_id
		JOIN notebook_dir d ON d.id = nw.dir_id
		JOIN notebook_dir ld ON ld.id = d.parent_id
		JOIN notebook nb ON nb.id = ld.notebook_id
		JOIN word w ON w.id = nw.word_id
		WHERE nb.user_id = $1 AND ld.name = $2
		ORDER BY s.id`, userID, "第"+strconv.Itoa(no)+"课")
	if err == nil {
		for nbRows.Next() {
			var ref nbRef
			var text string
			if nbRows.Scan(&ref.NotebookID, &ref.DirID, &ref.WordID, &ref.Surface, &text) == nil {
				key := normText(text)
				if _, ok := nbMatches[key]; !ok {
					nbMatches[key] = ref
				}
			}
		}
		nbRows.Close()
	}

	for pi := range points {
		for ei := range points[pi].Examples {
			e := &points[pi].Examples[ei]
			key := normText(e.Text)
			if lineNo, ok := recMatches[key]; ok && recID != "" {
				e.Rec = &recRef{LessonID: recID, LineNo: lineNo}
			}
			if ref, ok := nbMatches[key]; ok {
				r := ref
				e.Notebook = &r
			}
		}
	}

	httpx.WriteJSON(w, http.StatusOK, map[string]any{
		"book":                 book,
		"lesson_no":            no,
		"title":                title,
		"recitation_lesson_id": recID,
		"points":               points,
	})
}

// normText 归一化例句文本用于精确匹配：去空白与标点，保留文字本身。
func normText(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch r {
		case ' ', '\u3000', '、', '。', '，', '．', '！', '？', '!', '?',
			'「', '」', '『', '』', '（', '）', '(', ')', '・', '…', '：', ':', '；', ';', '～', '~':
			continue
		}
		b.WriteRune(r)
	}
	return b.String()
}
