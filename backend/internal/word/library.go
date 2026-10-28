// 生词库：word 表本身按用户持久（每次提交词单 upsert），
// Library 聚合出现课程数与首见/最近日期，供前端浏览与选词。

package word

import (
	"net/http"

	"github.com/greatgeek/TingShuo/backend/internal/auth"
	"github.com/greatgeek/TingShuo/backend/internal/httpx"
)

// Library 生词库列表。q 可选：模糊匹配 surface/reading/meaning。
func (h *Handler) Library(w http.ResponseWriter, r *http.Request) {
	userID := auth.FromContext(r.Context())
	q := r.URL.Query().Get("q")

	rows, err := h.pool.Query(r.Context(), `
		SELECT w.id, w.surface, w.reading, w.pos, w.meaning, w.mastery,
		       COUNT(DISTINCT lw.lesson_id)::int,
		       COALESCE(BOOL_OR(lw.is_key), false),
		       to_char(MIN(l.lesson_date), 'YYYY-MM-DD'),
		       to_char(MAX(l.lesson_date), 'YYYY-MM-DD')
		FROM word w
		LEFT JOIN lesson_word lw ON lw.word_id = w.id
		LEFT JOIN daily_lesson l ON l.id = lw.lesson_id
		WHERE w.user_id = $1
		  AND ($2 = '' OR w.surface ILIKE '%' || $2 || '%'
		       OR w.reading ILIKE '%' || $2 || '%' OR w.meaning ILIKE '%' || $2 || '%')
		GROUP BY w.id
		ORDER BY MAX(l.lesson_date) DESC NULLS LAST, w.created_at DESC`,
		userID, q)
	if err != nil {
		httpx.Err(w, httpx.Internal(err.Error()))
		return
	}
	defer rows.Close()

	type item struct {
		ID          string  `json:"id"`
		Surface     string  `json:"surface"`
		Reading     *string `json:"reading"`
		Pos         *string `json:"pos"`
		Meaning     *string `json:"meaning"`
		Mastery     string  `json:"mastery"`
		LessonCount int     `json:"lesson_count"`
		IsKey       bool    `json:"is_key"`
		FirstSeen   *string `json:"first_seen"`
		LastSeen    *string `json:"last_seen"`
	}
	items := []item{}
	for rows.Next() {
		var it item
		if err := rows.Scan(&it.ID, &it.Surface, &it.Reading, &it.Pos, &it.Meaning,
			&it.Mastery, &it.LessonCount, &it.IsKey, &it.FirstSeen, &it.LastSeen); err != nil {
			httpx.Err(w, httpx.Internal(err.Error()))
			return
		}
		items = append(items, it)
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"items": items, "count": len(items)})
}
