// Package review 的 HTTP 层：入池、到期列表、结果提交（FR-7）。
package review

import (
	"encoding/json"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/greatgeek/TingShuo/backend/internal/auth"
	"github.com/greatgeek/TingShuo/backend/internal/httpx"
	"github.com/greatgeek/TingShuo/backend/pkg/ulid"
	"github.com/jackc/pgx/v5/pgxpool"
)

type Handler struct{ pool *pgxpool.Pool }

func NewHandler(pool *pgxpool.Pool) *Handler { return &Handler{pool: pool} }

// Add POST /reviews {sentence_id, source} —— 入池（FR-7.4 三源 + 手动）。
// 一句一个复习项：已存在则重新激活（取消已掌握状态），不重复创建。
func (h *Handler) Add(w http.ResponseWriter, r *http.Request) {
	userID := auth.FromContext(r.Context())
	var req struct {
		SentenceID string `json:"sentence_id"`
		Source     string `json:"source"` // marked / blind_low / retell_stuck / manual
	}
	if e := httpx.Decode(r, &req); e != nil {
		httpx.Err(w, e)
		return
	}
	switch req.Source {
	case "marked", "blind_low", "retell_stuck", "manual":
	default:
		httpx.Err(w, httpx.BadRequest("SOURCE_INVALID",
			"source 仅支持 marked/blind_low/retell_stuck/manual"))
		return
	}

	// 归属校验：句子必须属于当前用户的某节课
	var exists bool
	if err := h.pool.QueryRow(r.Context(), `
		SELECT EXISTS (
			SELECT 1 FROM sentence s
			JOIN daily_lesson l ON l.id = s.lesson_id
			WHERE s.id = $1 AND l.user_id = $2)`,
		req.SentenceID, userID).Scan(&exists); err != nil || !exists {
		httpx.Err(w, httpx.NotFound("句子不存在或不属于当前用户"))
		return
	}

	// 首次入池 interval=1 → due = today+1（§8.5）
	_, err := h.pool.Exec(r.Context(), `
		INSERT INTO review_item (id, user_id, sentence_id, source, interval_days, due_date)
		VALUES ($1, $2, $3, $4, 1, CURRENT_DATE + 1)
		ON CONFLICT (sentence_id) DO UPDATE
		  SET mastered = false, source = EXCLUDED.source, updated_at = now()`,
		ulid.MustNew(), userID, req.SentenceID, req.Source)
	if err != nil {
		httpx.Err(w, httpx.Internal("入池失败: "+err.Error()))
		return
	}
	httpx.WriteJSON(w, http.StatusCreated, map[string]any{
		"sentence_id": req.SentenceID, "source": req.Source, "due_date": time.Now().AddDate(0, 0, 1).Format("2006-01-02"),
	})
}

// Due GET /reviews/due?date=YYYY-MM-DD —— 到期列表（FR-7.5，按到期时间排序）。
func (h *Handler) Due(w http.ResponseWriter, r *http.Request) {
	userID := auth.FromContext(r.Context())
	date := r.URL.Query().Get("date")
	if date == "" {
		date = time.Now().Format("2006-01-02")
	}

	rows, err := h.pool.Query(r.Context(), `
		SELECT rv.id, rv.sentence_id, rv.source, rv.interval_days, rv.due_date::text,
		       rv.error_count, rv.mastered, s.text, s.translation, s.speaker, s.lesson_id,
		       s.ruby_tokens, aa.audio_id
		FROM review_item rv
		JOIN sentence s ON s.id = rv.sentence_id
		LEFT JOIN LATERAL (
			SELECT a.id AS audio_id FROM audio_asset a
			WHERE a.sentence_id = s.id AND a.status = 'ready'
			ORDER BY (a.rate = 1.0) DESC LIMIT 1
		) aa ON true
		WHERE rv.user_id = $1 AND rv.mastered = false AND rv.due_date <= $2
		ORDER BY rv.due_date`, userID, date)
	if err != nil {
		httpx.Err(w, httpx.Internal(err.Error()))
		return
	}
	defer rows.Close()

	items := []map[string]any{}
	for rows.Next() {
		var id, sentenceID, source, dueDate, text, lessonID string
		var interval, errCount int
		var translation, speaker, audioID *string
		var mastered bool
		var ruby []byte
		if err := rows.Scan(&id, &sentenceID, &source, &interval, &dueDate,
			&errCount, &mastered, &text, &translation, &speaker, &lessonID,
			&ruby, &audioID); err != nil {
			httpx.Err(w, httpx.Internal(err.Error()))
			return
		}
		sentence := map[string]any{
			"text": text, "translation": translation, "speaker": speaker, "lesson_id": lessonID,
		}
		if len(ruby) > 0 {
			sentence["ruby_tokens"] = json.RawMessage(ruby)
		}
		items = append(items, map[string]any{
			"id": id, "sentence_id": sentenceID, "source": source,
			"interval_days": interval, "due_date": dueDate, "error_count": errCount,
			"audio_id": audioID,
			"sentence": sentence,
		})
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"items": items, "count": len(items)})
}

// Pool GET /reviews/pool —— 复习池句子 ID 列表（含已掌握，供前端渲染星标用）。
func (h *Handler) Pool(w http.ResponseWriter, r *http.Request) {
	userID := auth.FromContext(r.Context())
	rows, err := h.pool.Query(r.Context(), `
		SELECT sentence_id, mastered FROM review_item WHERE user_id = $1`, userID)
	if err != nil {
		httpx.Err(w, httpx.Internal(err.Error()))
		return
	}
	defer rows.Close()

	active := []string{}
	mastered := []string{}
	for rows.Next() {
		var sid string
		var m bool
		if err := rows.Scan(&sid, &m); err != nil {
			httpx.Err(w, httpx.Internal(err.Error()))
			return
		}
		if m {
			mastered = append(mastered, sid)
		} else {
			active = append(active, sid)
		}
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{
		"active": active, "mastered": mastered,
	})
}

// SubmitResult POST /reviews/{id}/result {result:"pass"|"fail"}
// —— 调度核心：NextInterval（§8.5），返回新 due_date。
func (h *Handler) SubmitResult(w http.ResponseWriter, r *http.Request) {
	userID := auth.FromContext(r.Context())
	reviewID := chi.URLParam(r, "id")

	var req struct {
		Result Result `json:"result"`
	}
	if e := httpx.Decode(r, &req); e != nil {
		httpx.Err(w, e)
		return
	}
	if req.Result != Pass && req.Result != Fail {
		httpx.Err(w, httpx.BadRequest("RESULT_INVALID", "result 仅支持 pass/fail"))
		return
	}

	var curInterval int
	err := h.pool.QueryRow(r.Context(), `
		SELECT interval_days FROM review_item
		WHERE id = $1 AND user_id = $2`, reviewID, userID).Scan(&curInterval)
	if err != nil {
		httpx.Err(w, httpx.NotFound("复习项不存在或不属于当前用户"))
		return
	}

	interval, dueDate, errDelta := ApplyResult(curInterval, req.Result, time.Now())
	var lastResult = string(req.Result)
	_, err = h.pool.Exec(r.Context(), `
		UPDATE review_item SET
			interval_days = $3, due_date = $4::date, last_result = $5,
			error_count = error_count + $6, updated_at = now()
		WHERE id = $1 AND user_id = $2`,
		reviewID, userID, interval, dueDate.Format("2006-01-02"), lastResult, errDelta)
	if err != nil {
		httpx.Err(w, httpx.Internal("更新复习状态失败: "+err.Error()))
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{
		"review_id": reviewID, "interval_days": interval,
		"due_date": dueDate.Format("2006-01-02"), "result": lastResult,
	})
}

// Master POST /reviews/{id}/master —— 标记已掌握，移出复习池（FR-7.7）。
func (h *Handler) Master(w http.ResponseWriter, r *http.Request) {
	userID := auth.FromContext(r.Context())
	reviewID := chi.URLParam(r, "id")
	tag, err := h.pool.Exec(r.Context(), `
		UPDATE review_item SET mastered = true, updated_at = now()
		WHERE id = $1 AND user_id = $2`, reviewID, userID)
	if err != nil {
		httpx.Err(w, httpx.Internal(err.Error()))
		return
	}
	if tag.RowsAffected() == 0 {
		httpx.Err(w, httpx.NotFound("复习项不存在或不属于当前用户"))
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"review_id": reviewID, "mastered": true})
}
