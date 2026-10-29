// Package notebook 生词本浏览：本列表、多级目录树、目录内词表（含绑定句子）。
// 词本身在全局 word 表（背词/复习/生词库共用），本包只负责组织结构与例句展示。
package notebook

import (
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/greatgeek/TingShuo/backend/internal/auth"
	"github.com/greatgeek/TingShuo/backend/internal/httpx"
	"github.com/jackc/pgx/v5/pgxpool"
)

type Handler struct{ pool *pgxpool.Pool }

func NewHandler(pool *pgxpool.Pool) *Handler { return &Handler{pool: pool} }

// List GET /notebooks：用户的生词本（含词数/目录数概览）。
func (h *Handler) List(w http.ResponseWriter, r *http.Request) {
	userID := auth.FromContext(r.Context())
	rows, err := h.pool.Query(r.Context(), `
		SELECT nb.id, nb.name,
		       (SELECT count(DISTINCT nw.word_id) FROM notebook_word nw WHERE nw.notebook_id = nb.id),
		       (SELECT count(*) FROM notebook_dir d WHERE d.notebook_id = nb.id)
		FROM notebook nb
		WHERE nb.user_id = $1
		ORDER BY nb.created_at`, userID)
	if err != nil {
		httpx.Err(w, httpx.Internal(err.Error()))
		return
	}
	defer rows.Close()
	type item struct {
		ID        string `json:"id"`
		Name      string `json:"name"`
		WordCount int    `json:"word_count"`
		DirCount  int    `json:"dir_count"`
	}
	items := []item{}
	for rows.Next() {
		var it item
		if err := rows.Scan(&it.ID, &it.Name, &it.WordCount, &it.DirCount); err != nil {
			httpx.Err(w, httpx.Internal(err.Error()))
			return
		}
		items = append(items, it)
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"items": items, "count": len(items)})
}

// Tree GET /notebooks/{id}/tree：多级目录树（含每目录词数），一次取回前端组装。
func (h *Handler) Tree(w http.ResponseWriter, r *http.Request) {
	userID := auth.FromContext(r.Context())
	nbID := chi.URLParam(r, "id")
	if !h.owns(r, nbID, userID) {
		httpx.Err(w, httpx.NotFound("生词本不存在或不属于当前用户"))
		return
	}
	rows, err := h.pool.Query(r.Context(), `
		SELECT d.id, d.parent_id, d.name, d.sort_order,
		       (SELECT count(*) FROM notebook_word nw WHERE nw.dir_id = d.id)
		FROM notebook_dir d
		WHERE d.notebook_id = $1
		ORDER BY d.sort_order, d.name`, nbID)
	if err != nil {
		httpx.Err(w, httpx.Internal(err.Error()))
		return
	}
	defer rows.Close()
	type node struct {
		ID        string  `json:"id"`
		ParentID  *string `json:"parent_id"`
		Name      string  `json:"name"`
		Sort      int     `json:"sort_order"`
		WordCount int     `json:"word_count"`
	}
	nodes := []node{}
	for rows.Next() {
		var n node
		if err := rows.Scan(&n.ID, &n.ParentID, &n.Name, &n.Sort, &n.WordCount); err != nil {
			httpx.Err(w, httpx.Internal(err.Error()))
			return
		}
		nodes = append(nodes, n)
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"dirs": nodes})
}

// DirWords GET /notebooks/dir/{dir_id}/words：目录内词 + 绑定句子（浏览/点播）。
func (h *Handler) DirWords(w http.ResponseWriter, r *http.Request) {
	userID := auth.FromContext(r.Context())
	dirID := chi.URLParam(r, "dir_id")
	var nbID string
	err := h.pool.QueryRow(r.Context(), `
		SELECT d.notebook_id FROM notebook_dir d
		JOIN notebook nb ON nb.id = d.notebook_id
		WHERE d.id = $1 AND nb.user_id = $2`, dirID, userID).Scan(&nbID)
	if err != nil {
		httpx.Err(w, httpx.NotFound("目录不存在或不属于当前用户"))
		return
	}
	rows, err := h.pool.Query(r.Context(), `
		SELECT nw.id, w.id, w.surface, w.reading, w.pos, w.meaning, nw.sort_order
		FROM notebook_word nw
		JOIN word w ON w.id = nw.word_id
		WHERE nw.dir_id = $1
		ORDER BY nw.sort_order, w.surface`, dirID)
	if err != nil {
		httpx.Err(w, httpx.Internal(err.Error()))
		return
	}
	defer rows.Close()
	type sent struct {
		Text        string  `json:"text"`
		Translation *string `json:"translation"`
		Src         *string `json:"src"`
	}
	type item struct {
		LinkID    string  `json:"link_id"`
		WordID    string  `json:"word_id"`
		Surface   string  `json:"surface"`
		Reading   *string `json:"reading"`
		Pos       *string `json:"pos"`
		Meaning   *string `json:"meaning"`
		Sort      int     `json:"sort_order"`
		Sentences []sent  `json:"sentences"`
	}
	items := []item{}
	index := map[string]int{}
	for rows.Next() {
		var it item
		if err := rows.Scan(&it.LinkID, &it.WordID, &it.Surface, &it.Reading, &it.Pos, &it.Meaning, &it.Sort); err != nil {
			httpx.Err(w, httpx.Internal(err.Error()))
			return
		}
		it.Sentences = []sent{}
		index[it.LinkID] = len(items)
		items = append(items, it)
	}
	// 句子单独查一次按 link 分组（避免 GROUP_CONCAT 拼接破坏结构）
	srows, err := h.pool.Query(r.Context(), `
		SELECT s.notebook_word_id, s.text, s.translation, s.src
		FROM notebook_sentence s
		JOIN notebook_word nw ON nw.id = s.notebook_word_id
		WHERE nw.dir_id = $1
		ORDER BY s.sort_order`, dirID)
	if err != nil {
		httpx.Err(w, httpx.Internal(err.Error()))
		return
	}
	defer srows.Close()
	for srows.Next() {
		var linkID, text string
		var trans, src *string
		if err := srows.Scan(&linkID, &text, &trans, &src); err != nil {
			httpx.Err(w, httpx.Internal(err.Error()))
			return
		}
		if i, ok := index[linkID]; ok {
			items[i].Sentences = append(items[i].Sentences, sent{Text: text, Translation: trans, Src: src})
		}
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"items": items, "count": len(items)})
}

func (h *Handler) owns(r *http.Request, nbID, userID string) bool {
	var n int
	_ = h.pool.QueryRow(r.Context(),
		`SELECT count(*) FROM notebook WHERE id = $1 AND user_id = $2`, nbID, userID).Scan(&n)
	return n > 0
}
