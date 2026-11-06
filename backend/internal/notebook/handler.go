// Package notebook 生词本浏览：本列表、多级目录树、目录内词表（含绑定句子）。
// 词本身在全局 word 表（背词/复习/生词库共用），本包只负责组织结构与例句展示。
package notebook

import (
	"encoding/json"
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
		Text        string          `json:"text"`
		Ruby        json.RawMessage `json:"ruby_tokens"`
		Translation *string         `json:"translation"`
		Src         *string         `json:"src"`
	}
	type item struct {
		LinkID    string          `json:"link_id"`
		WordID    string          `json:"word_id"`
		Surface   string          `json:"surface"`
		Reading   *string         `json:"reading"`
		Ruby      json.RawMessage `json:"ruby_tokens"` // 逐块振假名（kagome 切块+本子读音切分）
		Pos       *string         `json:"pos"`
		Meaning   *string         `json:"meaning"`
		Sort      int             `json:"sort_order"`
		Sentences []sent          `json:"sentences"`
	}
	items := []item{}
	index := map[string]int{}
	for rows.Next() {
		var it item
		if err := rows.Scan(&it.LinkID, &it.WordID, &it.Surface, &it.Reading, &it.Pos, &it.Meaning, &it.Sort); err != nil {
			httpx.Err(w, httpx.Internal(err.Error()))
			return
		}
		if it.Reading != nil {
			if toks := wordRubyTokens(it.Surface, *it.Reading); toks != nil {
				b, _ := json.Marshal(toks)
				it.Ruby = b
			}
		}
		it.Sentences = []sent{}
		index[it.LinkID] = len(items)
		items = append(items, it)
	}
	// 句子单独查一次按 link 分组（避免 GROUP_CONCAT 拼接破坏结构）
	srows, err := h.pool.Query(r.Context(), `
		SELECT s.notebook_word_id, s.text, s.ruby_tokens, s.translation, s.src
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
		var ruby []byte
		var trans, src *string
		if err := srows.Scan(&linkID, &text, &ruby, &trans, &src); err != nil {
			httpx.Err(w, httpx.Internal(err.Error()))
			return
		}
		if i, ok := index[linkID]; ok {
			items[i].Sentences = append(items[i].Sentences, sent{
				Text: text, Ruby: json.RawMessage(ruby), Translation: trans, Src: src,
			})
		}
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"items": items, "count": len(items)})
}

// CopyWords GET /notebooks/dir/{dir_id}/copy?scope=subtree|self —— 目录选择面板的复制按钮：
// 课节点（scope=subtree）复制整课全部子孙单元的词与例句，单元节点（self）只复制自己。
// 返回按目录排序的平铺条目（带 dir 名），前端按目录分组拼文本。
func (h *Handler) CopyWords(w http.ResponseWriter, r *http.Request) {
	userID := auth.FromContext(r.Context())
	dirID := chi.URLParam(r, "dir_id")
	scope := r.URL.Query().Get("scope")
	if scope != "subtree" {
		scope = "self"
	}
	var title string
	err := h.pool.QueryRow(r.Context(), `
		SELECT d.name FROM notebook_dir d
		JOIN notebook nb ON nb.id = d.notebook_id
		WHERE d.id = $1 AND nb.user_id = $2`, dirID, userID).Scan(&title)
	if err != nil {
		httpx.Err(w, httpx.NotFound("目录不存在或不属于当前用户"))
		return
	}
	rows, err := h.pool.Query(r.Context(), `
		WITH RECURSIVE ds AS (
			SELECT id, sort_order FROM notebook_dir WHERE id = $1
			UNION ALL
			SELECT c.id, c.sort_order FROM notebook_dir c JOIN ds ON c.parent_id = ds.id
		)
		SELECT ds.sort_order, d.name, w.surface, w.reading, w.pos, w.meaning,
		       s.text, s.translation
		FROM notebook_word nw
		JOIN ds ON ds.id = nw.dir_id
		JOIN notebook_dir d ON d.id = nw.dir_id
		JOIN word w ON w.id = nw.word_id
		LEFT JOIN notebook_sentence s ON s.notebook_word_id = nw.id
		WHERE ($2 = 'subtree' OR nw.dir_id = $1)
		ORDER BY ds.sort_order, nw.sort_order, w.surface, s.sort_order`, dirID, scope)
	if err != nil {
		httpx.Err(w, httpx.Internal(err.Error()))
		return
	}
	defer rows.Close()
	type csent struct {
		Text        string  `json:"text"`
		Translation *string `json:"translation"`
	}
	type citem struct {
		Dir       string  `json:"dir"`
		Surface   string  `json:"surface"`
		Reading   *string `json:"reading"`
		Pos       *string `json:"pos"`
		Meaning   *string `json:"meaning"`
		Sentences []csent `json:"sentences"`
	}
	items := []citem{}
	seen := map[string]int{} // key = dir+"\x00"+surface 防同词多句重复建条目
	for rows.Next() {
		var dirSort int
		var dir, surface string
		var reading, pos, meaning, text, trans *string
		if err := rows.Scan(&dirSort, &dir, &surface, &reading, &pos, &meaning, &text, &trans); err != nil {
			httpx.Err(w, httpx.Internal(err.Error()))
			return
		}
		key := dir + "\x00" + surface
		i, ok := seen[key]
		if !ok {
			i = len(items)
			seen[key] = i
			items = append(items, citem{Dir: dir, Surface: surface, Reading: reading, Pos: pos, Meaning: meaning, Sentences: []csent{}})
		}
		if text != nil && *text != "" {
			items[i].Sentences = append(items[i].Sentences, csent{Text: *text, Translation: trans})
		}
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"title": title, "count": len(items), "items": items})
}

func (h *Handler) owns(r *http.Request, nbID, userID string) bool {
	var n int
	_ = h.pool.QueryRow(r.Context(),
		`SELECT count(*) FROM notebook WHERE id = $1 AND user_id = $2`, nbID, userID).Scan(&n)
	return n > 0
}
