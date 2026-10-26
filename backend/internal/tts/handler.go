// TTS 资产接口（BYOK FR-3.12）：check（全局去重判定）→ presign（上传 URL）→
// register（登记 audio_asset）→ file（302 预签名下载）。
package tts

import (
	"context"
	"net/http"
	"regexp"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/greatgeek/TingShuo/backend/internal/auth"
	"github.com/greatgeek/TingShuo/backend/internal/httpx"
	"github.com/greatgeek/TingShuo/backend/pkg/storage"
	"github.com/greatgeek/TingShuo/backend/pkg/ulid"
	"github.com/jackc/pgx/v5/pgxpool"
)

type Handler struct {
	pool  *pgxpool.Pool
	store *storage.Client
}

func NewHandler(pool *pgxpool.Pool, store *storage.Client) *Handler {
	return &Handler{pool: pool, store: store}
}

var hexKey = regexp.MustCompile(`^[0-9a-f]{64}$`)

// ---- POST /audio/check：批量缓存判定（客户端先查再决定是否调 Azure，AC-3.1）----

type checkItem struct {
	SentenceID string  `json:"sentence_id"`
	Text       string  `json:"text"`
	Voice      string  `json:"voice"`
	Rate       float64 `json:"rate"`
	Style      string  `json:"style"`
	EngineVer  string  `json:"engine_ver"`
}

func (h *Handler) Check(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Items []checkItem `json:"items"`
	}
	if e := httpx.Decode(r, &req); e != nil {
		httpx.Err(w, e)
		return
	}
	if len(req.Items) == 0 || len(req.Items) > 200 {
		httpx.Err(w, httpx.BadRequest("ITEMS_INVALID", "items 为 1—200 条"))
		return
	}
	userID := auth.FromContext(r.Context())

	// 归属校验 + 计算 cache_key
	type entry struct {
		Item     checkItem
		CacheKey string
	}
	entries := make([]entry, 0, len(req.Items))
	keys := make([]string, 0, len(req.Items))
	for _, it := range req.Items {
		var exists bool
		if err := h.pool.QueryRow(r.Context(), `
			SELECT EXISTS (
				SELECT 1 FROM sentence s
				JOIN daily_lesson l ON l.id = s.lesson_id
				WHERE s.id = $1 AND l.user_id = $2)`,
			it.SentenceID, userID).Scan(&exists); err != nil || !exists {
			httpx.Err(w, httpx.NotFound("句子不存在或不属于当前用户: "+it.SentenceID))
			return
		}
		ck := CacheKey(it.Text, Params{Voice: it.Voice, Rate: it.Rate, Style: it.Style, EngineVer: it.EngineVer})
		entries = append(entries, entry{it, ck})
		keys = append(keys, ck)
	}

	// 一次查全部命中资产
	rows, err := h.pool.Query(r.Context(),
		`SELECT cache_key, id FROM audio_asset WHERE cache_key = ANY($1) AND status = 'ready'`, keys)
	if err != nil {
		httpx.Err(w, httpx.Internal(err.Error()))
		return
	}
	defer rows.Close()
	found := map[string]string{}
	for rows.Next() {
		var ck, id string
		if err := rows.Scan(&ck, &id); err != nil {
			httpx.Err(w, httpx.Internal(err.Error()))
			return
		}
		found[ck] = id
	}

	out := make([]map[string]any, 0, len(entries))
	for _, e := range entries {
		item := map[string]any{
			"sentence_id": e.Item.SentenceID,
			"cache_key":   e.CacheKey,
			"exists":      false,
		}
		if id, ok := found[e.CacheKey]; ok {
			item["exists"] = true
			item["audio_id"] = id
		}
		out = append(out, item)
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"items": out})
}

// ---- POST /audio/presign：预签名上传 URL ----

func (h *Handler) Presign(w http.ResponseWriter, r *http.Request) {
	var req struct {
		CacheKey string `json:"cache_key"`
	}
	if e := httpx.Decode(r, &req); e != nil {
		httpx.Err(w, e)
		return
	}
	if !hexKey.MatchString(req.CacheKey) {
		httpx.Err(w, httpx.BadRequest("CACHE_KEY_INVALID", "cache_key 必须为 64 位十六进制"))
		return
	}
	url, err := h.store.PresignedPUT(r.Context(), req.CacheKey, 10*time.Minute)
	if err != nil {
		httpx.Err(w, httpx.Internal("预签名生成失败: "+err.Error()))
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{
		"upload_url": url, "expires_in": 600, "object_key": storage.AudioKey(req.CacheKey),
	})
}

// ---- POST /audio/register：客户端上传完成后登记资产 ----

func (h *Handler) Register(w http.ResponseWriter, r *http.Request) {
	userID := auth.FromContext(r.Context())
	var req struct {
		SentenceID string  `json:"sentence_id"`
		CacheKey   string  `json:"cache_key"`
		Voice      string  `json:"voice"`
		Rate       float64 `json:"rate"`
		Style      string  `json:"style"`
		EngineVer  string  `json:"engine_ver"`
		DurationMs int     `json:"duration_ms"`
		SizeBytes  int     `json:"size_bytes"`
	}
	if e := httpx.Decode(r, &req); e != nil {
		httpx.Err(w, e)
		return
	}
	if !hexKey.MatchString(req.CacheKey) || req.Voice == "" || req.SentenceID == "" {
		httpx.Err(w, httpx.BadRequest("PARAMS_INVALID", "cache_key/voice/sentence_id 不能为空"))
		return
	}

	// 归属校验
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

	// 全局去重：cache_key 冲突时复用既有资产（不同课程/用户的相同句子也命中）
	var audioID string
	err := h.pool.QueryRow(r.Context(), `
		INSERT INTO audio_asset (id, sentence_id, cache_key, voice, rate, style,
		                         format, duration_ms, size_bytes, storage_url, status, engine_ver, generated_at)
		VALUES ($1, $2, $3, $4, $5, $6, 'mp3', $7, $8, $9, 'ready', $10, now())
		ON CONFLICT (cache_key) DO UPDATE SET generated_at = now()
		RETURNING id`,
		ulid.MustNew(), req.SentenceID, req.CacheKey, req.Voice, req.Rate, req.Style,
		req.DurationMs, req.SizeBytes, storage.AudioKey(req.CacheKey), req.EngineVer).Scan(&audioID)
	if err != nil {
		httpx.Err(w, httpx.Internal("资产登记失败: "+err.Error()))
		return
	}

	// 成本观测（部署文档 §9.3）
	_, _ = h.pool.Exec(r.Context(), `
		INSERT INTO tts_call_log (id, user_id, sentence_id, voice, chars, duration_ms, cache_hit)
		VALUES ($1, $2, $3, $4, 0, $5, true)`,
		ulid.MustNew(), userID, req.SentenceID, req.Voice, req.DurationMs)

	httpx.WriteJSON(w, http.StatusCreated, map[string]any{"audio_id": audioID, "status": "ready"})
}

// ---- GET /audio/{id}/file：校验归属后 302 预签名 GET（10 分钟，§8.6）----

func (h *Handler) File(w http.ResponseWriter, r *http.Request) {
	userID := auth.FromContext(r.Context())
	audioID := chi.URLParam(r, "id")

	var objectKey string
	err := h.pool.QueryRow(r.Context(), `
		SELECT a.storage_url FROM audio_asset a
		JOIN sentence s ON s.id = a.sentence_id
		JOIN daily_lesson l ON l.id = s.lesson_id
		WHERE a.id = $1 AND l.user_id = $2 AND a.status = 'ready'`,
		audioID, userID).Scan(&objectKey)
	if err != nil {
		httpx.Err(w, httpx.NotFound("音频不存在或不属于当前用户"))
		return
	}

	url, err := h.store.PresignedGET(context.Background(), objectKey, 10*time.Minute)
	if err != nil {
		httpx.Err(w, httpx.Internal("预签名生成失败: "+err.Error()))
		return
	}
	http.Redirect(w, r, url, http.StatusFound) // 302
}
