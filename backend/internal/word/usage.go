// Package word 实现生词维度的用法索引（FR-2.15 数据能力）：
// 词 → 其生成的短语（material.type='phrases'）与出现的句子（对话/短文），
// 句子经 material 关联到场景与标题，可直接点播。
package word

import (
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/greatgeek/TingShuo/backend/internal/auth"
	"github.com/greatgeek/TingShuo/backend/internal/httpx"
	"github.com/jackc/pgx/v5/pgxpool"
)

type Handler struct{ pool *pgxpool.Pool }

func NewHandler(pool *pgxpool.Pool) *Handler { return &Handler{pool: pool} }

// Usage GET /words/{id}/usage：词的用法索引（短语 + 句子 + 场景 + 音频），按日期倒序。
func (h *Handler) Usage(w http.ResponseWriter, r *http.Request) {
	userID := auth.FromContext(r.Context())
	wordID := chi.URLParam(r, "id")

	// 归属校验：词属于当前用户
	var surface, reading *string
	err := h.pool.QueryRow(r.Context(), `
		SELECT surface, reading FROM word WHERE id = $1 AND user_id = $2`,
		wordID, userID).Scan(&surface, &reading)
	if err != nil {
		httpx.Err(w, httpx.NotFound("生词不存在或不属于当前用户"))
		return
	}

	rows, err := h.pool.Query(r.Context(), `
		SELECT s.id, s.text, s.ruby_tokens, s.translation,
		       m.type, m.scene, m.title,
		       to_char(l.lesson_date, 'YYYY-MM-DD'),
		       aa.audio_id
		FROM word_sentence ws
		JOIN sentence s     ON s.id = ws.sentence_id
		JOIN material m     ON m.id = s.material_id
		JOIN daily_lesson l ON l.id = ws.lesson_id
		LEFT JOIN LATERAL (
			SELECT a.id AS audio_id FROM audio_asset a
			WHERE a.sentence_id = s.id AND a.status = 'ready'
			ORDER BY (a.rate = 1.0) DESC LIMIT 1
		) aa ON true
		WHERE ws.word_id = $1
		ORDER BY l.lesson_date DESC, m.type, m.sort_order, s.sort_order`, wordID)
	if err != nil {
		httpx.Err(w, httpx.Internal(err.Error()))
		return
	}
	defer rows.Close()

	type item struct {
		SentenceID string  `json:"sentence_id"`
		Text       string  `json:"text"`
		Ruby       []byte  `json:"ruby_tokens"`
		Translation *string `json:"translation"`
		Type       string  `json:"type"`  // phrases=短语 / dialogue / passage
		Scene      *string `json:"scene"`
		Title      *string `json:"title"`
		Date       string  `json:"lesson_date"`
		AudioID    *string `json:"audio_id"` // 可空：前端点播时自动 synthesize
	}
	items := []item{}
	seenText := map[string]bool{} // 展示层按文本去重（容错存量重复数据；新落库已在上游去重）
	for rows.Next() {
		var it item
		if err := rows.Scan(&it.SentenceID, &it.Text, &it.Ruby, &it.Translation,
			&it.Type, &it.Scene, &it.Title, &it.Date, &it.AudioID); err != nil {
			httpx.Err(w, httpx.Internal(err.Error()))
			return
		}
		if seenText[it.Text] {
			continue
		}
		seenText[it.Text] = true
		items = append(items, it)
	}

	httpx.WriteJSON(w, http.StatusOK, map[string]any{
		"word_id": wordID,
		"surface": surface,
		"reading": reading,
		"items":   items,
		"count":   len(items),
	})
}
