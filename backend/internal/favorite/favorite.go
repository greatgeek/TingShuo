// Package favorite 实现句子与材料收藏（FR-12.3：句卡可单独听、循环、连播全部收藏）。
package favorite

import (
	"encoding/json"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/greatgeek/TingShuo/backend/internal/auth"
	"github.com/greatgeek/TingShuo/backend/internal/httpx"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

type Handler struct{ pool *pgxpool.Pool }

func NewHandler(pool *pgxpool.Pool) *Handler { return &Handler{pool: pool} }

// AddSentence POST /favorites {sentence_id}：收藏句子（幂等）。
func (h *Handler) AddSentence(w http.ResponseWriter, r *http.Request) {
	userID := auth.FromContext(r.Context())
	var req struct {
		SentenceID string `json:"sentence_id"`
	}
	if e := httpx.Decode(r, &req); e != nil {
		httpx.Err(w, e)
		return
	}
	// 归属校验：只能收藏自己课程里的句子
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
	if _, err := h.pool.Exec(r.Context(), `
		INSERT INTO favorite_sentence (user_id, sentence_id) VALUES ($1, $2)
		ON CONFLICT (user_id, sentence_id) DO NOTHING`,
		userID, req.SentenceID); err != nil {
		httpx.Err(w, httpx.Internal("收藏失败: "+err.Error()))
		return
	}
	httpx.WriteJSON(w, http.StatusCreated, map[string]string{"sentence_id": req.SentenceID, "status": "favorited"})
}

// RemoveSentence DELETE /favorites/sentences/{sentence_id}：取消收藏。
func (h *Handler) RemoveSentence(w http.ResponseWriter, r *http.Request) {
	userID := auth.FromContext(r.Context())
	if _, err := h.pool.Exec(r.Context(),
		`DELETE FROM favorite_sentence WHERE user_id = $1 AND sentence_id = $2`,
		userID, chi.URLParam(r, "sentence_id")); err != nil {
		httpx.Err(w, httpx.Internal(err.Error()))
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// ListSentences GET /favorites/sentences：收藏句列表（含句子内容与首个可用音频 id，可空）。
func (h *Handler) ListSentences(w http.ResponseWriter, r *http.Request) {
	userID := auth.FromContext(r.Context())
	rows, err := h.pool.Query(r.Context(), `
		SELECT f.sentence_id, s.text, s.ruby_tokens, s.translation,
		       to_char(l.lesson_date, 'YYYY-MM-DD'), aa.audio_id
		FROM favorite_sentence f
		JOIN sentence s ON s.id = f.sentence_id
		JOIN daily_lesson l ON l.id = s.lesson_id
		LEFT JOIN LATERAL (
			SELECT a.id AS audio_id FROM audio_asset a
			WHERE a.sentence_id = s.id AND a.status = 'ready'
			ORDER BY (a.rate = 1.0) DESC LIMIT 1
		) aa ON true
		WHERE f.user_id = $1
		ORDER BY f.created_at DESC`, userID)
	if err != nil {
		httpx.Err(w, httpx.Internal(err.Error()))
		return
	}
	defer rows.Close()

	items := []map[string]any{}
	for rows.Next() {
		var sid, text, lessonDate string
		var ruby []byte
		var translation, audioID *string
		if err := rows.Scan(&sid, &text, &ruby, &translation, &lessonDate, &audioID); err != nil {
			httpx.Err(w, httpx.Internal(err.Error()))
			return
		}
		items = append(items, map[string]any{
			"sentence_id": sid,
			"text":        text,
			"ruby_tokens": json.RawMessage(ruby),
			"translation": translation,
			"lesson_date": lessonDate,
			"audio_id":    audioID, // 可空：播放时前端调 synthesize 现场配音
		})
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"items": items, "count": len(items)})
}

// ToggleMaterial POST /favorites/materials {material_id}：材料收藏切换（收藏/取消）。
func (h *Handler) ToggleMaterial(w http.ResponseWriter, r *http.Request) {
	userID := auth.FromContext(r.Context())
	var req struct {
		MaterialID string `json:"material_id"`
	}
	if e := httpx.Decode(r, &req); e != nil {
		httpx.Err(w, e)
		return
	}
	var exists bool
	if err := h.pool.QueryRow(r.Context(), `
		SELECT EXISTS (
			SELECT 1 FROM material m
			JOIN daily_lesson l ON l.id = m.lesson_id
			WHERE m.id = $1 AND l.user_id = $2)`,
		req.MaterialID, userID).Scan(&exists); err != nil || !exists {
		httpx.Err(w, httpx.NotFound("材料不存在或不属于当前用户"))
		return
	}

	var favorited bool
	var current string
	err := h.pool.QueryRow(r.Context(),
		`SELECT material_id FROM favorite_material WHERE user_id = $1 AND material_id = $2`,
		userID, req.MaterialID).Scan(&current)
	switch {
	case err == pgx.ErrNoRows: // 未收藏 → 收藏
		if _, err := h.pool.Exec(r.Context(),
			`INSERT INTO favorite_material (user_id, material_id) VALUES ($1, $2)`,
			userID, req.MaterialID); err != nil {
			httpx.Err(w, httpx.Internal(err.Error()))
			return
		}
		favorited = true
	case err != nil:
		httpx.Err(w, httpx.Internal(err.Error()))
		return
	default: // 已收藏 → 取消
		if _, err := h.pool.Exec(r.Context(),
			`DELETE FROM favorite_material WHERE user_id = $1 AND material_id = $2`,
			userID, req.MaterialID); err != nil {
			httpx.Err(w, httpx.Internal(err.Error()))
			return
		}
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"material_id": req.MaterialID, "favorited": favorited})
}

// ListMaterials GET /favorites/materials：已收藏材料 ID 列表。
func (h *Handler) ListMaterials(w http.ResponseWriter, r *http.Request) {
	userID := auth.FromContext(r.Context())
	rows, err := h.pool.Query(r.Context(),
		`SELECT material_id FROM favorite_material WHERE user_id = $1`, userID)
	if err != nil {
		httpx.Err(w, httpx.Internal(err.Error()))
		return
	}
	defer rows.Close()
	ids := []string{}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			httpx.Err(w, httpx.Internal(err.Error()))
			return
		}
		ids = append(ids, id)
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"material_ids": ids})
}
