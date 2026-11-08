// Package recitation 课文本：整段原声课文 + SRT 行时间轴，逐行背诵/听写进度。
// 课文内容是全局的（所有用户共享），进度按 (user, lesson, line) 存。
package recitation

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"

	"github.com/go-chi/chi/v5"
	"github.com/greatgeek/TingShuo/backend/internal/auth"
	"github.com/greatgeek/TingShuo/backend/internal/httpx"
	"github.com/greatgeek/TingShuo/backend/pkg/storage"
	"github.com/jackc/pgx/v5/pgxpool"
)

type Handler struct {
	pool  *pgxpool.Pool
	store *storage.Client
}

func NewHandler(pool *pgxpool.Pool, store *storage.Client) *Handler {
	return &Handler{pool: pool, store: store}
}

type lessonItem struct {
	ID            string `json:"id"`
	LessonNo      int    `json:"lesson_no"`
	Title         string `json:"title"`
	DurationMS    int    `json:"duration_ms"`
	LineCount     int    `json:"line_count"`
	RecitedCount  int    `json:"recited_count"`
	DictCount     int    `json:"dict_count"`
}

// List GET /recitation/lessons：课文本列表（含当前用户的背诵/听写进度）。
func (h *Handler) List(w http.ResponseWriter, r *http.Request) {
	userID := auth.FromContext(r.Context())
	rows, err := h.pool.Query(r.Context(), `
		SELECT l.id, l.lesson_no, l.title, l.duration_ms,
		       (SELECT count(*) FROM recitation_line rl WHERE rl.lesson_id = l.id),
		       COALESCE(p.recited, 0), COALESCE(p.dict, 0)
		FROM recitation_lesson l
		LEFT JOIN (
			SELECT lesson_id,
			       count(*) FILTER (WHERE recited_at IS NOT NULL) AS recited,
			       count(*) FILTER (WHERE dict_ok_at IS NOT NULL OR dict_reading_at IS NOT NULL) AS dict
			FROM recitation_progress WHERE user_id = $1
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
			&it.LineCount, &it.RecitedCount, &it.DictCount); err != nil {
			httpx.Err(w, httpx.Internal(err.Error()))
			return
		}
		items = append(items, it)
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"items": items, "count": len(items)})
}

type lineItem struct {
	ID          string          `json:"id"`
	LineNo      int             `json:"line_no"`
	StartMS     int             `json:"start_ms"`
	EndMS       int             `json:"end_ms"`
	Text        string          `json:"text"`
	Translation string          `json:"translation"`
	RubyTokens  json.RawMessage `json:"ruby_tokens"`
	Speakers    []string        `json:"speakers"`
	Scene       string          `json:"scene"`
	Recited     bool            `json:"recited"`
	DictOK      bool            `json:"dict_ok"`
	DictReading bool            `json:"dict_reading"`
}

// Detail GET /recitation/lessons/{id}：课文行（含振り仮名与当前用户逐行进度）。
func (h *Handler) Detail(w http.ResponseWriter, r *http.Request) {
	userID := auth.FromContext(r.Context())
	lessonID := chi.URLParam(r, "id")
	var lesson lessonItem
	if err := h.pool.QueryRow(r.Context(),
		`SELECT id, lesson_no, title, duration_ms FROM recitation_lesson WHERE id = $1`, lessonID).
		Scan(&lesson.ID, &lesson.LessonNo, &lesson.Title, &lesson.DurationMS); err != nil {
		httpx.Err(w, httpx.NotFound("课文本不存在"))
		return
	}
	rows, err := h.pool.Query(r.Context(), `
		SELECT rl.id, rl.line_no, rl.start_ms, rl.end_ms, rl.text, rl.translation, rl.ruby_tokens,
		       rl.speakers, rl.scene,
		       (p.recited_at IS NOT NULL), (p.dict_ok_at IS NOT NULL), (p.dict_reading_at IS NOT NULL)
		FROM recitation_line rl
		LEFT JOIN recitation_progress p
		  ON p.lesson_id = rl.lesson_id AND p.line_no = rl.line_no AND p.user_id = $2
		WHERE rl.lesson_id = $1
		ORDER BY rl.line_no`, lessonID, userID)
	if err != nil {
		httpx.Err(w, httpx.Internal(err.Error()))
		return
	}
	defer rows.Close()
	lines := []lineItem{}
	for rows.Next() {
		var it lineItem
		var ruby, speakers []byte
		if err := rows.Scan(&it.ID, &it.LineNo, &it.StartMS, &it.EndMS, &it.Text,
			&it.Translation, &ruby, &speakers, &it.Scene, &it.Recited, &it.DictOK, &it.DictReading); err != nil {
			httpx.Err(w, httpx.Internal(err.Error()))
			return
		}
		if len(ruby) > 0 {
			it.RubyTokens = json.RawMessage(ruby)
		}
		it.Speakers = []string{}
		if len(speakers) > 0 {
			_ = json.Unmarshal(speakers, &it.Speakers)
		}
		lines = append(lines, it)
	}
	lesson.LineCount = len(lines)
	for _, l := range lines {
		if l.Recited {
			lesson.RecitedCount++
		}
		if l.DictOK || l.DictReading {
			lesson.DictCount++
		}
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"lesson": lesson, "lines": lines})
}

type progressReq struct {
	LineNo int    `json:"line_no"`
	Kind   string `json:"kind"`   // recite=背诵自评；dict=听写
	Result string `json:"result"` // dict 时 ok=汉字/原文对；reading=假名读音对
}

// Progress POST /recitation/lessons/{id}/progress：记录一行「会背了」或听写通过。
func (h *Handler) Progress(w http.ResponseWriter, r *http.Request) {
	userID := auth.FromContext(r.Context())
	lessonID := chi.URLParam(r, "id")
	var req progressReq
	if e := httpx.Decode(r, &req); e != nil {
		httpx.Err(w, e)
		return
	}
	var exists int
	if err := h.pool.QueryRow(r.Context(),
		`SELECT count(1) FROM recitation_line WHERE lesson_id = $1 AND line_no = $2`,
		lessonID, req.LineNo).Scan(&exists); err != nil || exists == 0 {
		httpx.Err(w, httpx.BadRequest("BAD_LINE", "课文行不存在"))
		return
	}
	var err error
	switch {
	case req.Kind == "recite":
		_, err = h.pool.Exec(r.Context(), `
			INSERT INTO recitation_progress (user_id, lesson_id, line_no, recited_at)
			VALUES ($1, $2, $3, now())
			ON CONFLICT (user_id, lesson_id, line_no)
			DO UPDATE SET recited_at = now(), updated_at = now()`,
			userID, lessonID, req.LineNo)
	case req.Kind == "dict" && req.Result == "ok":
		_, err = h.pool.Exec(r.Context(), `
			INSERT INTO recitation_progress (user_id, lesson_id, line_no, dict_ok_at)
			VALUES ($1, $2, $3, now())
			ON CONFLICT (user_id, lesson_id, line_no)
			DO UPDATE SET dict_ok_at = now(), updated_at = now()`,
			userID, lessonID, req.LineNo)
	case req.Kind == "dict" && req.Result == "reading":
		_, err = h.pool.Exec(r.Context(), `
			INSERT INTO recitation_progress (user_id, lesson_id, line_no, dict_reading_at)
			VALUES ($1, $2, $3, now())
			ON CONFLICT (user_id, lesson_id, line_no)
			DO UPDATE SET dict_reading_at = now(), updated_at = now()`,
			userID, lessonID, req.LineNo)
	case req.Kind == "reset":
		_, err = h.pool.Exec(r.Context(),
			`DELETE FROM recitation_progress WHERE user_id = $1 AND lesson_id = $2 AND line_no = $3`,
			userID, lessonID, req.LineNo)
	default:
		httpx.Err(w, httpx.BadRequest("BAD_KIND", "kind/result 不合法"))
		return
	}
	if err != nil {
		httpx.Err(w, httpx.Internal(err.Error()))
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// Audio GET /recitation/lessons/{id}/audio：整段课文音频（支持 Range）。
func (h *Handler) Audio(w http.ResponseWriter, r *http.Request) {
	lessonID := chi.URLParam(r, "id")
	var key string
	if err := h.pool.QueryRow(r.Context(),
		`SELECT audio_key FROM recitation_lesson WHERE id = $1`, lessonID).Scan(&key); err != nil {
		httpx.Err(w, httpx.NotFound("课文本不存在"))
		return
	}
	objectKey := storage.AudioKey(key)
	size, err := h.store.StatAudio(r.Context(), objectKey)
	if err != nil {
		httpx.Err(w, httpx.BadGateway("对象存储读取失败: "+err.Error()))
		return
	}
	start, end, ranged := parseRange(r.Header.Get("Range"), size)
	obj, err := h.store.GetObjectRange(r.Context(), objectKey, start, end)
	if err != nil {
		httpx.Err(w, httpx.BadGateway("对象存储读取失败: "+err.Error()))
		return
	}
	defer obj.Close()
	w.Header().Set("Content-Type", "audio/mpeg")
	w.Header().Set("Accept-Ranges", "bytes")
	w.Header().Set("Cache-Control", "private, max-age=86400")
	w.Header().Set("Content-Length", strconv.FormatInt(end-start+1, 10))
	if ranged {
		w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, size))
		w.WriteHeader(http.StatusPartialContent)
	}
	io.Copy(w, obj)
}

// parseRange 解析 Range 头为 [start, end] 闭区间；无/非法 Range 返回整对象。
func parseRange(hdr string, size int64) (start, end int64, ranged bool) {
	end = size - 1
	if !strings.HasPrefix(hdr, "bytes=") {
		return 0, end, false
	}
	spec := strings.SplitN(strings.TrimPrefix(hdr, "bytes="), "-", 2)
	if len(spec) != 2 {
		return 0, end, false
	}
	if spec[0] == "" {
		n, err := strconv.ParseInt(spec[1], 10, 64)
		if err != nil || n <= 0 {
			return 0, end, false
		}
		if n > size {
			n = size
		}
		return size - n, end, true
	}
	s, err := strconv.ParseInt(spec[0], 10, 64)
	if err != nil || s < 0 || s >= size {
		return 0, end, false
	}
	start = s
	if spec[1] != "" {
		if e, err := strconv.ParseInt(spec[1], 10, 64); err == nil && e >= start && e < size {
			end = e
		}
	}
	return start, end, true
}
