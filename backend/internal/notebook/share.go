// 生词本共享（资源版块）与收藏：共享本对所有人可见；收藏分只读关注（live）
// 与复制独立副本（copy）两种。复制在服务端完成，含目录树、词与例句。
package notebook

import (
	"fmt"
	"context"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"
	"github.com/greatgeek/TingShuo/backend/internal/auth"
	"github.com/greatgeek/TingShuo/backend/internal/httpx"
	"github.com/greatgeek/TingShuo/backend/pkg/ulid"
	"github.com/jackc/pgx/v5"
)

const ownerLabelSQL = `COALESCE(NULLIF(u.nickname, ''), split_part(u.email, '@', 1))`

// Share POST /notebooks/{id}/share {on, intro}：上架/下架共享。
func (h *Handler) Share(w http.ResponseWriter, r *http.Request) {
	userID := auth.FromContext(r.Context())
	nbID := chi.URLParam(r, "id")
	if !h.owns(r, nbID, userID) {
		httpx.Err(w, httpx.NotFound("生词本不存在或不属于当前用户"))
		return
	}
	var req struct {
		On    bool   `json:"on"`
		Intro string `json:"intro"`
	}
	if e := httpx.Decode(r, &req); e != nil {
		httpx.Err(w, e)
		return
	}
	intro := strings.TrimSpace(req.Intro)
	if runeLen(intro) > 200 {
		httpx.Err(w, httpx.BadRequest("BAD_INTRO", "简介最多 200 个字符"))
		return
	}
	if req.On {
		_, err := h.pool.Exec(r.Context(), `
			INSERT INTO notebook_share (notebook_id, owner_id, intro)
			VALUES ($1, $2, $3)
			ON CONFLICT (notebook_id) DO UPDATE SET intro = EXCLUDED.intro`,
			nbID, userID, intro)
		if err != nil {
			httpx.Err(w, httpx.Internal(err.Error()))
			return
		}
	} else {
		if _, err := h.pool.Exec(r.Context(),
			`DELETE FROM notebook_share WHERE notebook_id = $1 AND owner_id = $2`, nbID, userID); err != nil {
			httpx.Err(w, httpx.Internal(err.Error()))
			return
		}
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"shared": req.On})
}

// ListShared GET /shared/notebooks：资源版块列表（含我的收藏状态与收藏数）。
func (h *Handler) ListShared(w http.ResponseWriter, r *http.Request) {
	userID := auth.FromContext(r.Context())
	rows, err := h.pool.Query(r.Context(), `
		SELECT nb.id, nb.name, `+ownerLabelSQL+`,
		       (SELECT count(*) FROM notebook_dir d WHERE d.notebook_id = nb.id),
		       (SELECT count(DISTINCT nw.word_id) FROM notebook_word nw WHERE nw.notebook_id = nb.id),
		       (SELECT count(*) FROM notebook_favorite f WHERE f.share_notebook_id = nb.id),
		       s.created_at, s.intro, (u.id = $1),
		       COALESCE(f.mode, ''), f.copied_notebook_id
		FROM notebook_share s
		JOIN notebook nb ON nb.id = s.notebook_id
		JOIN "user" u ON u.id = s.owner_id
		LEFT JOIN notebook_favorite f
		       ON f.share_notebook_id = s.notebook_id AND f.user_id = $1
		ORDER BY s.created_at DESC`, userID)
	if err != nil {
		httpx.Err(w, httpx.Internal(err.Error()))
		return
	}
	defer rows.Close()
	type item struct {
		ID               string  `json:"id"`
		Name             string  `json:"name"`
		Owner            string  `json:"owner"`
		DirCount         int     `json:"dir_count"`
		WordCount        int     `json:"word_count"`
		FavCount         int     `json:"fav_count"`
		Intro            string  `json:"intro"`
		IsMine           bool    `json:"is_mine"`
		MyMode           string  `json:"my_mode"`
		CopiedNotebookID *string `json:"copied_notebook_id"`
	}
	items := []item{}
	for rows.Next() {
		var it item
		var created interface{}
		if err := rows.Scan(&it.ID, &it.Name, &it.Owner, &it.DirCount, &it.WordCount,
			&it.FavCount, &created, &it.Intro, &it.IsMine, &it.MyMode, &it.CopiedNotebookID); err != nil {
			httpx.Err(w, httpx.Internal(err.Error()))
			return
		}
		items = append(items, it)
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"items": items, "count": len(items)})
}

// SharedDetail GET /shared/notebooks/{id}：共享本概览 + 目录树（只读浏览）。
func (h *Handler) SharedDetail(w http.ResponseWriter, r *http.Request) {
	userID := auth.FromContext(r.Context())
	nbID := chi.URLParam(r, "id")
	var name, owner, intro string
	var isMine bool
	var myMode string
	var copied *string
	err := h.pool.QueryRow(r.Context(), `
		SELECT nb.name, `+ownerLabelSQL+`, s.intro, (u.id = $2),
		       COALESCE(f.mode, ''), f.copied_notebook_id
		FROM notebook_share s
		JOIN notebook nb ON nb.id = s.notebook_id
		JOIN "user" u ON u.id = s.owner_id
		LEFT JOIN notebook_favorite f
		       ON f.share_notebook_id = s.notebook_id AND f.user_id = $2
		WHERE s.notebook_id = $1`, nbID, userID).Scan(&name, &owner, &intro, &isMine, &myMode, &copied)
	if err != nil {
		httpx.Err(w, httpx.NotFound("共享生词本不存在或已下架"))
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
	dirs := []node{}
	for rows.Next() {
		var n node
		if err := rows.Scan(&n.ID, &n.ParentID, &n.Name, &n.Sort, &n.WordCount); err != nil {
			httpx.Err(w, httpx.Internal(err.Error()))
			return
		}
		dirs = append(dirs, n)
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{
		"id": nbID, "name": name, "owner": owner, "intro": intro,
		"is_mine": isMine, "my_mode": myMode, "copied_notebook_id": copied,
		"dirs": dirs,
	})
}

// SharedDirWords GET /shared/notebooks/{id}/dir/{dir_id}/words：只读词表。
func (h *Handler) SharedDirWords(w http.ResponseWriter, r *http.Request) {
	nbID := chi.URLParam(r, "id")
	dirID := chi.URLParam(r, "dir_id")
	var ok bool
	if err := h.pool.QueryRow(r.Context(), `
		SELECT EXISTS (
			SELECT 1 FROM notebook_dir d
			JOIN notebook_share s ON s.notebook_id = d.notebook_id
			WHERE d.id = $1 AND d.notebook_id = $2)`, dirID, nbID).Scan(&ok); err != nil {
		httpx.Err(w, httpx.Internal(err.Error()))
		return
	}
	if !ok {
		httpx.Err(w, httpx.NotFound("目录不存在或共享已下架"))
		return
	}
	items, e := h.loadDirWords(r, dirID)
	if e != nil {
		httpx.Err(w, e)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"items": items, "count": len(items)})
}

// Favorite POST /shared/notebooks/{id}/favorite {mode:"live"|"copy"}。
// copy 模式在同一事务里把共享本完整复制到当前账号。
func (h *Handler) Favorite(w http.ResponseWriter, r *http.Request) {
	userID := auth.FromContext(r.Context())
	nbID := chi.URLParam(r, "id")
	var req struct {
		Mode string `json:"mode"`
	}
	if e := httpx.Decode(r, &req); e != nil {
		httpx.Err(w, e)
		return
	}
	if req.Mode != "live" && req.Mode != "copy" {
		httpx.Err(w, httpx.BadRequest("BAD_MODE", "收藏方式不正确"))
		return
	}
	var ownerID string
	if err := h.pool.QueryRow(r.Context(),
		`SELECT owner_id FROM notebook_share WHERE notebook_id = $1`, nbID).Scan(&ownerID); err != nil {
		httpx.Err(w, httpx.NotFound("共享生词本不存在或已下架"))
		return
	}
	if ownerID == userID {
		httpx.Err(w, httpx.BadRequest("OWN_SHARE", "这是你共享的生词本，无需收藏"))
		return
	}
	var exists bool
	_ = h.pool.QueryRow(r.Context(),
		`SELECT EXISTS (SELECT 1 FROM notebook_favorite WHERE user_id = $1 AND share_notebook_id = $2)`,
		userID, nbID).Scan(&exists)
	if exists {
		httpx.Err(w, httpx.Conflict("ALREADY_FAVORITED", "已经收藏过这个生词本"))
		return
	}

	if req.Mode == "live" {
		_, err := h.pool.Exec(r.Context(), `
			INSERT INTO notebook_favorite (id, user_id, share_notebook_id, mode)
			VALUES ($1, $2, $3, 'live')`, ulid.MustNew(), userID, nbID)
		if err != nil {
			if isUniqueViolation(err) {
				httpx.Err(w, httpx.Conflict("ALREADY_FAVORITED", "已经收藏过这个生词本"))
				return
			}
			httpx.Err(w, httpx.Internal(err.Error()))
			return
		}
		httpx.WriteJSON(w, http.StatusOK, map[string]any{"mode": "live"})
		return
	}

	ctx := r.Context()
	tx, err := h.pool.Begin(ctx)
	if err != nil {
		httpx.Err(w, httpx.Internal(err.Error()))
		return
	}
	defer tx.Rollback(ctx)
	newID, err := copyNotebook(ctx, tx, nbID, userID)
	if err != nil {
		httpx.Err(w, httpx.Internal(fmt.Sprintf("复制失败: %v", err)))
		return
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO notebook_favorite (id, user_id, share_notebook_id, mode, copied_notebook_id)
		VALUES ($1, $2, $3, 'copy', $4)`, ulid.MustNew(), userID, nbID, newID); err != nil {
		httpx.Err(w, httpx.Internal(err.Error()))
		return
	}
	if err := tx.Commit(ctx); err != nil {
		httpx.Err(w, httpx.Internal(err.Error()))
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"mode": "copy", "notebook_id": newID})
}

// Unfavorite DELETE /shared/notebooks/{id}/favorite：取关/取消收藏。
// copy 模式只解除收藏关系，已复制的生词本保留为用户自己的本。
func (h *Handler) Unfavorite(w http.ResponseWriter, r *http.Request) {
	userID := auth.FromContext(r.Context())
	nbID := chi.URLParam(r, "id")
	if _, err := h.pool.Exec(r.Context(),
		`DELETE FROM notebook_favorite WHERE user_id = $1 AND share_notebook_id = $2`, userID, nbID); err != nil {
		httpx.Err(w, httpx.Internal(err.Error()))
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// Favorites GET /notebooks/favorites：我的收藏（生词本屏顶部卡片区）。
func (h *Handler) Favorites(w http.ResponseWriter, r *http.Request) {
	userID := auth.FromContext(r.Context())
	rows, err := h.pool.Query(r.Context(), `
		SELECT f.mode, f.copied_notebook_id, s.notebook_id, nb.name, `+ownerLabelSQL+`,
		       (SELECT count(*) FROM notebook_dir d WHERE d.notebook_id = nb.id),
		       (SELECT count(DISTINCT nw.word_id) FROM notebook_word nw WHERE nw.notebook_id = nb.id),
		       (SELECT count(*) FROM notebook_favorite f2 WHERE f2.share_notebook_id = nb.id)
		FROM notebook_favorite f
		JOIN notebook_share s ON s.notebook_id = f.share_notebook_id
		JOIN notebook nb ON nb.id = s.notebook_id
		JOIN "user" u ON u.id = s.owner_id
		WHERE f.user_id = $1
		ORDER BY f.created_at DESC`, userID)
	if err != nil {
		httpx.Err(w, httpx.Internal(err.Error()))
		return
	}
	defer rows.Close()
	type item struct {
		Mode             string  `json:"mode"`
		CopiedNotebookID *string `json:"copied_notebook_id"`
		ID               string  `json:"id"`
		Name             string  `json:"name"`
		Owner            string  `json:"owner"`
		DirCount         int     `json:"dir_count"`
		WordCount        int     `json:"word_count"`
		FavCount         int     `json:"fav_count"`
	}
	items := []item{}
	for rows.Next() {
		var it item
		if err := rows.Scan(&it.Mode, &it.CopiedNotebookID, &it.ID, &it.Name, &it.Owner,
			&it.DirCount, &it.WordCount, &it.FavCount); err != nil {
			httpx.Err(w, httpx.Internal(err.Error()))
			return
		}
		items = append(items, it)
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"items": items, "count": len(items)})
}

// copyNotebook 在同一事务内把源本复制到目标用户：本 → 目录树 → 词（按用户去重）→ 例句。
func copyNotebook(ctx context.Context, tx pgx.Tx, srcID, dstUser string) (string, error) {
	var baseName string
	if err := tx.QueryRow(ctx, `SELECT name FROM notebook WHERE id = $1`, srcID).Scan(&baseName); err != nil {
		return "", err
	}
	newID, err := insertCopiedNotebook(ctx, tx, dstUser, baseName)
	if err != nil {
		return "", err
	}

	// 目录：按深度顺序插入，保证父目录先落库
	dirRows, err := tx.Query(ctx, `
		WITH RECURSIVE ds AS (
			SELECT id, parent_id, name, sort_order, 0 AS depth
			FROM notebook_dir WHERE notebook_id = $1 AND parent_id IS NULL
			UNION ALL
			SELECT c.id, c.parent_id, c.name, c.sort_order, ds.depth + 1
			FROM notebook_dir c JOIN ds ON c.parent_id = ds.id
		)
		SELECT id, parent_id, name, sort_order FROM ds ORDER BY depth, sort_order`, srcID)
	if err != nil {
		return "", err
	}
	type srcDir struct {
		id     string
		parent *string
		name   string
		sort   int
	}
	var dirs []srcDir
	for dirRows.Next() {
		var d srcDir
		if err := dirRows.Scan(&d.id, &d.parent, &d.name, &d.sort); err != nil {
			dirRows.Close()
			return "", err
		}
		dirs = append(dirs, d)
	}
	dirRows.Close()
	if err := dirRows.Err(); err != nil {
		return "", err
	}
	dirMap := make(map[string]string, len(dirs))
	for _, d := range dirs {
		var parent *string
		if d.parent != nil {
			mapped := dirMap[*d.parent]
			parent = &mapped
		}
		nid := ulid.MustNew()
		if _, err := tx.Exec(ctx, `
			INSERT INTO notebook_dir (id, notebook_id, parent_id, name, sort_order)
			VALUES ($1, $2, $3, $4, $5)`, nid, newID, parent, d.name, d.sort); err != nil {
			return "", err
		}
		dirMap[d.id] = nid
	}

	// 词：目标用户已有同词（surface+reading）则复用，否则新建词
	wordRows, err := tx.Query(ctx, `
		SELECT nw.id, nw.dir_id, nw.sort_order, w.surface, w.reading, w.pos, w.meaning
		FROM notebook_word nw
		JOIN word w ON w.id = nw.word_id
		WHERE nw.notebook_id = $1
		ORDER BY nw.dir_id, nw.sort_order, w.surface`, srcID)
	if err != nil {
		return "", err
	}
	type srcWord struct {
		linkID string
		dirID  string
		sort   int
		surf   string
		read   *string
		pos    *string
		mean   *string
	}
	var words []srcWord
	for wordRows.Next() {
		var w srcWord
		if err := wordRows.Scan(&w.linkID, &w.dirID, &w.sort, &w.surf, &w.read, &w.pos, &w.mean); err != nil {
			wordRows.Close()
			return "", err
		}
		words = append(words, w)
	}
	wordRows.Close()
	if err := wordRows.Err(); err != nil {
		return "", err
	}
	linkMap := make(map[string]string, len(words))
	for _, w := range words {
		var wid string
		err := tx.QueryRow(ctx, `
			SELECT id FROM word
			WHERE user_id = $1 AND surface = $2 AND reading IS NOT DISTINCT FROM $3
			LIMIT 1`, dstUser, w.surf, w.read).Scan(&wid)
		if err == pgx.ErrNoRows {
			wid = ulid.MustNew()
			if _, err := tx.Exec(ctx, `
				INSERT INTO word (id, user_id, surface, reading, pos, meaning)
				VALUES ($1, $2, $3, $4, $5, $6)`,
				wid, dstUser, w.surf, w.read, w.pos, w.mean); err != nil {
				return "", err
			}
		} else if err != nil {
			return "", err
		}
		nlid := ulid.MustNew()
		if _, err := tx.Exec(ctx, `
			INSERT INTO notebook_word (id, notebook_id, dir_id, word_id, sort_order)
			VALUES ($1, $2, $3, $4, $5)`, nlid, newID, dirMap[w.dirID], wid, w.sort); err != nil {
			return "", err
		}
		linkMap[w.linkID] = nlid
	}

	// 例句
	sentRows, err := tx.Query(ctx, `
		SELECT s.notebook_word_id, s.text, s.ruby_tokens, s.translation, s.src, s.sort_order
		FROM notebook_sentence s
		JOIN notebook_word nw ON nw.id = s.notebook_word_id
		WHERE nw.notebook_id = $1`, srcID)
	if err != nil {
		return "", err
	}
	type srcSent struct {
		linkID, text string
		ruby         []byte
		trans, src   *string
		sort         int
	}
	var sents []srcSent
	for sentRows.Next() {
		var s srcSent
		if err := sentRows.Scan(&s.linkID, &s.text, &s.ruby, &s.trans, &s.src, &s.sort); err != nil {
			sentRows.Close()
			return "", err
		}
		sents = append(sents, s)
	}
	sentRows.Close()
	if err := sentRows.Err(); err != nil {
		return "", err
	}
	for _, s := range sents {
		nlid, ok := linkMap[s.linkID]
		if !ok {
			continue
		}
		if _, err := tx.Exec(ctx, `
			INSERT INTO notebook_sentence
				(id, notebook_word_id, text, ruby_tokens, translation, src, sort_order)
			VALUES ($1, $2, $3, $4, $5, $6, $7)`,
			ulid.MustNew(), nlid, s.text, s.ruby, s.trans, s.src, s.sort); err != nil {
			return "", err
		}
	}
	return newID, nil
}

// insertCopiedNotebook 先在已用名单里挑一个不重名的候选再插入：
// 事务内撞唯一约束会让整个事务失效，不能靠报错后重试。
func insertCopiedNotebook(ctx context.Context, tx pgx.Tx, dstUser, base string) (string, error) {
	rows, err := tx.Query(ctx, `SELECT name FROM notebook WHERE user_id = $1`, dstUser)
	if err != nil {
		return "", err
	}
	used := map[string]bool{}
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			rows.Close()
			return "", err
		}
		used[n] = true
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return "", err
	}
	for i := 0; i < 100; i++ {
		name := base
		switch i {
		case 0:
		case 1:
			name = truncRunes(base, 34) + "（收藏）"
		default:
			name = truncRunes(base, 28) + fmt.Sprintf("（收藏 %d）", i)
		}
		if used[name] {
			continue
		}
		id := ulid.MustNew()
		if _, err := tx.Exec(ctx, `INSERT INTO notebook (id, user_id, name) VALUES ($1, $2, $3)`,
			id, dstUser, name); err != nil {
			return "", err
		}
		return id, nil
	}
	return "", fmt.Errorf("同名生词本过多")
}

func truncRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n])
}
