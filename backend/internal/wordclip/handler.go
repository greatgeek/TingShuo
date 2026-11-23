package wordclip

import (
	"net/http"
	"strconv"
	"strings"

	"github.com/go-chi/chi/v5"
	"github.com/greatgeek/TingShuo/backend/internal/auth"
	"github.com/greatgeek/TingShuo/backend/internal/httpx"
	"github.com/jackc/pgx/v5/pgxpool"
)

type Handler struct {
	pool      *pgxpool.Pool
	cli       *Client
	mediaRoot string
}

func NewHandler(pool *pgxpool.Pool) *Handler {
	return &Handler{pool: pool, cli: NewClient()}
}

type clipItem struct {
	SegmentID    string  `json:"segment_id"`
	TextJa       string  `json:"text_ja"`
	TextZh       *string `json:"text_zh"`
	TextEn       string  `json:"text_en"`
	ZhSource     string  `json:"zh_source"`
	MatchStart   int     `json:"match_start"`
	MatchEnd     int     `json:"match_end"`
	MatchText    string  `json:"match_text"`
	StartMs      int     `json:"start_ms"`
	EndMs        int     `json:"end_ms"`
	DurationMs   int     `json:"duration_ms"`
	MediaID      string  `json:"media_id"`
	MediaName    string  `json:"media_name"`
	Episode      int     `json:"episode"`
	EpisodeTitle string  `json:"episode_title"`
	ImageURL     string  `json:"image_url"`
	AudioURL     string  `json:"audio_url"`
	VideoURL     string  `json:"video_url"`
}

// Clips GET /words/{id}/clips —— 生词的原声例句（最多 6 条）。
// 未生成时返回 enabled/pending，前端据此决定隐藏还是提示"准备中"。
func (h *Handler) Clips(w http.ResponseWriter, r *http.Request) {
	userID := auth.FromContext(r.Context())
	wordID := chi.URLParam(r, "id")

	var key string
	if err := h.pool.QueryRow(r.Context(), `
		SELECT word_clip_key(surface, reading) FROM word
		WHERE id = $1 AND user_id = $2`, wordID, userID).Scan(&key); err != nil {
		httpx.Err(w, httpx.NotFound("生词不存在或不属于当前用户"))
		return
	}

	rows, err := h.pool.Query(r.Context(), `
		SELECT segment_id, text_ja, text_zh, text_en, zh_source,
		       match_start, match_end, match_text, start_ms, end_ms,
		       media_id, media_name, episode, episode_title,
		       image_url, audio_url, video_url
		FROM word_clip
		WHERE word_key = $1
		ORDER BY sort_order
		LIMIT $2`, key, maxClips)
	if err != nil {
		httpx.Err(w, httpx.Internal(err.Error()))
		return
	}
	defer rows.Close()
	items := []clipItem{}
	for rows.Next() {
		var it clipItem
		if err := rows.Scan(&it.SegmentID, &it.TextJa, &it.TextZh, &it.TextEn, &it.ZhSource,
			&it.MatchStart, &it.MatchEnd, &it.MatchText, &it.StartMs, &it.EndMs,
			&it.MediaID, &it.MediaName, &it.Episode, &it.EpisodeTitle,
			&it.ImageURL, &it.AudioURL, &it.VideoURL); err != nil {
			httpx.Err(w, httpx.Internal(err.Error()))
			return
		}
		it.DurationMs = it.EndMs - it.StartMs
		// 媒体改走服务端同源缓存转发（浏览器带不了 Authorization，且部分网络直连 CDN 被拦）
		if h.mediaRoot != "" {
			if it.ImageURL != "" {
				it.ImageURL = clipMediaPath("image", it.SegmentID, it.ImageURL)
			}
			if it.AudioURL != "" {
				it.AudioURL = clipMediaPath("audio", it.SegmentID, it.AudioURL)
			}
			if it.VideoURL != "" {
				it.VideoURL = clipMediaPath("video", it.SegmentID, it.VideoURL)
			}
		}
		items = append(items, it)
	}

	status := ""
	expanded := false
	_ = h.pool.QueryRow(r.Context(),
		`SELECT status, expanded FROM word_clip_queue WHERE word_key = $1`, key).
		Scan(&status, &expanded)
	pending := status == "pending" || status == "running"
	httpx.WriteJSON(w, http.StatusOK, map[string]any{
		"items":      items,
		"count":      len(items),
		"enabled":    h.cli != nil,
		"pending":    pending,
		"status":     status,
		"expanded":   expanded,
		"max":        maxClips,
		"can_expand": h.cli != nil && len(items) > 0 && len(items) < maxClips && !expanded && !pending,
	})
}

// Enqueue POST /words/{id}/clips —— 生词本里按需生成原声例句。
// 已有缓存直接返回；无缓存则入队（跳过/失败的词可重试），前端轮询 Clips 拿结果。
func (h *Handler) Enqueue(w http.ResponseWriter, r *http.Request) {
	if h.cli == nil {
		httpx.Err(w, httpx.NewError(http.StatusServiceUnavailable, "CLIPS_DISABLED",
			"原声例句服务未配置（NADE_API_KEY）"))
		return
	}
	userID := auth.FromContext(r.Context())
	wordID := chi.URLParam(r, "id")

	var key, surface, reading, meaning string
	if err := h.pool.QueryRow(r.Context(), `
		SELECT word_clip_key(surface, reading), surface,
		       COALESCE(reading, ''), COALESCE(meaning, '')
		FROM word WHERE id = $1 AND user_id = $2`, wordID, userID).
		Scan(&key, &surface, &reading, &meaning); err != nil {
		httpx.Err(w, httpx.NotFound("生词不存在或不属于当前用户"))
		return
	}

	var count int
	_ = h.pool.QueryRow(r.Context(),
		`SELECT count(*) FROM word_clip WHERE word_key = $1`, key).Scan(&count)
	if count > 0 {
		if r.URL.Query().Get("more") != "1" {
			httpx.WriteJSON(w, http.StatusOK, map[string]any{
				"count": count, "pending": false, "status": "done",
			})
			return
		}
		var status string
		var expanded bool
		rowErr := h.pool.QueryRow(r.Context(),
			`SELECT status, expanded FROM word_clip_queue WHERE word_key = $1`, key).
			Scan(&status, &expanded)
		if rowErr == nil && (expanded || count >= maxClips) {
			httpx.WriteJSON(w, http.StatusOK, map[string]any{
				"count": count, "pending": false, "status": status,
				"expanded": expanded, "can_expand": false,
			})
			return
		}
		if rowErr == nil && (status == "pending" || status == "running") {
			httpx.WriteJSON(w, http.StatusOK, map[string]any{
				"count": count, "pending": true, "status": status,
				"expanded": false, "can_expand": false,
			})
			return
		}
		// 老词只补抓一次：拿满 18 条或补到没有新片段后 expanded 置真
		tag, err := h.pool.Exec(r.Context(), `
			INSERT INTO word_clip_queue (word_key, surface, reading, meaning, status, expand_pending)
			VALUES ($1, $2, $3, $4, 'pending', true)
			ON CONFLICT (word_key) DO UPDATE
			SET status = 'pending', expand_pending = true, attempts = 0,
			    last_error = '', created_at = now(), updated_at = now()
			WHERE word_clip_queue.expanded = false
			  AND word_clip_queue.status NOT IN ('pending', 'running')`,
			key, surface, reading, meaning)
		if err != nil {
			httpx.Err(w, httpx.Internal(err.Error()))
			return
		}
		if tag.RowsAffected() == 0 {
			var nowStatus string
			var nowExpanded bool
			_ = h.pool.QueryRow(r.Context(),
				`SELECT status, expanded FROM word_clip_queue WHERE word_key = $1`, key).
				Scan(&nowStatus, &nowExpanded)
			httpx.WriteJSON(w, http.StatusOK, map[string]any{
				"count": count, "pending": nowStatus == "pending" || nowStatus == "running",
				"status": nowStatus, "expanded": nowExpanded, "can_expand": false,
			})
			return
		}
		httpx.WriteJSON(w, http.StatusOK, map[string]any{
			"count": count, "pending": true, "status": "pending",
			"expanded": false, "can_expand": false,
		})
		return
	}

	tag, err := h.pool.Exec(r.Context(), `
		INSERT INTO word_clip_queue (word_key, surface, reading, meaning, status)
		VALUES ($1, $2, $3, $4, 'pending')
		ON CONFLICT (word_key) DO NOTHING`, key, surface, reading, meaning)
	if err != nil {
		httpx.Err(w, httpx.Internal(err.Error()))
		return
	}
	if tag.RowsAffected() == 0 {
		// 旧记录：跳过/失败/完成但无片段的词重新排队；pending/running 不动
		if _, err := h.pool.Exec(r.Context(), `
			UPDATE word_clip_queue
			SET status = 'pending', attempts = 0, last_error = '',
			    created_at = now(), updated_at = now()
			WHERE word_key = $1 AND status IN ('skipped', 'failed', 'done')`, key); err != nil {
			httpx.Err(w, httpx.Internal(err.Error()))
			return
		}
	}
	var status string
	_ = h.pool.QueryRow(r.Context(),
		`SELECT status FROM word_clip_queue WHERE word_key = $1`, key).Scan(&status)
	httpx.WriteJSON(w, http.StatusOK, map[string]any{
		"count":   count,
		"pending": status == "pending" || status == "running",
		"status":  status,
	})
}

// Backfill POST /words/clips/backfill?limit=100 —— 把当前用户尚无声例句的词入队。
// 试点/补量用；新词在课创建时已自动入队。返回本次入队数与全局待处理数。
func (h *Handler) Backfill(w http.ResponseWriter, r *http.Request) {
	if h.cli == nil {
		httpx.Err(w, httpx.NewError(http.StatusServiceUnavailable, "CLIPS_DISABLED",
			"原声例句服务未配置（NADE_API_KEY）"))
		return
	}
	userID := auth.FromContext(r.Context())
	limit := 100
	if v := strings.TrimSpace(r.URL.Query().Get("limit")); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			limit = n
		}
	}
	if limit > 500 {
		limit = 500
	}
	tag, err := h.pool.Exec(r.Context(), `
		INSERT INTO word_clip_queue (word_key, surface, reading, meaning)
		SELECT word_clip_key(w.surface, w.reading), w.surface,
		       COALESCE(w.reading, ''), COALESCE(w.meaning, '')
		FROM word w
		WHERE w.user_id = $1
		  AND NOT EXISTS (SELECT 1 FROM word_clip c
		                  WHERE c.word_key = word_clip_key(w.surface, w.reading))
		  AND NOT EXISTS (SELECT 1 FROM word_clip_queue q
		                  WHERE q.word_key = word_clip_key(w.surface, w.reading))
		ORDER BY w.created_at DESC
		LIMIT $2
		ON CONFLICT DO NOTHING`, userID, limit)
	if err != nil {
		httpx.Err(w, httpx.Internal(err.Error()))
		return
	}
	var pending int
	_ = h.pool.QueryRow(r.Context(),
		`SELECT count(*) FROM word_clip_queue WHERE status IN ('pending', 'running')`).Scan(&pending)
	httpx.WriteJSON(w, http.StatusOK, map[string]any{
		"enqueued": tag.RowsAffected(),
		"pending":  pending,
	})
}
