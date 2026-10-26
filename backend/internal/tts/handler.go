// TTS 资产接口。2026-10-27 架构决策（用户）：配音由**服务端**完成——
// synthesize 一步生成+落盘+登记（前端 CORS/代理/传输全部消失），前端仅缓存播放；
// 原浏览器 BYOK 直连形态（FR-3.12）调整，缓存去重护栏（AC-3.1）不变。
// check / presign / register 端点保留（历史数据与兼容），file 不变。
package tts

import (
	"io"
	"net/http"
	"regexp"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/greatgeek/TingShuo/backend/internal/auth"
	"github.com/greatgeek/TingShuo/backend/internal/httpx"
	"github.com/greatgeek/TingShuo/backend/pkg/storage"
	"github.com/greatgeek/TingShuo/backend/pkg/ttsclient"
	"github.com/greatgeek/TingShuo/backend/pkg/ulid"
	"github.com/jackc/pgx/v5/pgxpool"
)

type Handler struct {
	pool  *pgxpool.Pool
	store *storage.Client
	azure *ttsclient.Client
}

func NewHandler(pool *pgxpool.Pool, store *storage.Client, azure *ttsclient.Client) *Handler {
	return &Handler{pool: pool, store: store, azure: azure}
}

var hexKey = regexp.MustCompile(`^[0-9a-f]{64}$`)

// Synthesize POST /audio/synthesize {sentence_id, voice, rate}：
// 服务端生成该句 TTS 音频并登记资产，返回 audio_id。
// 幂等：cache_key 命中直接返回既有资产（AC-3.1：同句同参数零重复计费）。
func (h *Handler) Synthesize(w http.ResponseWriter, r *http.Request) {
	userID := auth.FromContext(r.Context())
	var req struct {
		SentenceID string  `json:"sentence_id"`
		Voice      string  `json:"voice"`
		Rate       float64 `json:"rate"`
	}
	if e := httpx.Decode(r, &req); e != nil {
		httpx.Err(w, e)
		return
	}
	if req.SentenceID == "" {
		httpx.Err(w, httpx.BadRequest("PARAMS_INVALID", "sentence_id 不能为空"))
		return
	}
	if req.Voice == "" {
		req.Voice = "Nanami" // FR-3.1 默认音色
	}
	if req.Rate == 0 {
		req.Rate = 1.0
	}

	// 归属校验 + 取句子文本（服务端为准，不信任前端文本）
	var text string
	err := h.pool.QueryRow(r.Context(), `
		SELECT s.text FROM sentence s
		JOIN daily_lesson l ON l.id = s.lesson_id
		WHERE s.id = $1 AND l.user_id = $2`,
		req.SentenceID, userID).Scan(&text)
	if err != nil {
		httpx.Err(w, httpx.NotFound("句子不存在或不属于当前用户"))
		return
	}

	// 缓存判定（FR-3.5）
	ck := CacheKey(text, Params{Voice: req.Voice, Rate: req.Rate, Style: "", EngineVer: "azure-v1"})
	var existingID string
	if err := h.pool.QueryRow(r.Context(),
		`SELECT id FROM audio_asset WHERE cache_key = $1 AND status = 'ready'`, ck).
		Scan(&existingID); err == nil {
		httpx.WriteJSON(w, http.StatusOK, map[string]any{"audio_id": existingID, "cached": true})
		return
	}

	start := time.Now()
	audio, serr := h.azure.Synthesize(r.Context(), text, req.Voice, req.Rate)
	if serr != nil {
		httpx.Err(w, httpx.BadGateway("TTS 生成失败: " + serr.Error()))
		return
	}
	if perr := h.store.PutAudio(r.Context(), ck, audio); perr != nil {
		httpx.Err(w, httpx.BadGateway("对象存储写入失败: " + perr.Error()))
		return
	}

	var audioID string
	err = h.pool.QueryRow(r.Context(), `
		INSERT INTO audio_asset (id, sentence_id, cache_key, voice, rate, style,
		                         format, duration_ms, size_bytes, storage_url, status, engine_ver, generated_at)
		VALUES ($1, $2, $3, $4, $5, '', 'mp3', 0, $6, $7, 'ready', 'azure-v1', now())
		ON CONFLICT (cache_key) DO UPDATE SET generated_at = now()
		RETURNING id`,
		ulid.MustNew(), req.SentenceID, ck, req.Voice, req.Rate, len(audio),
		storage.AudioKey(ck)).Scan(&audioID)
	if err != nil {
		httpx.Err(w, httpx.Internal("资产登记失败: " + err.Error()))
		return
	}

	// 成本观测（部署文档 §9.3）
	_, _ = h.pool.Exec(r.Context(), `
		INSERT INTO tts_call_log (id, user_id, sentence_id, voice, chars, duration_ms, cache_hit)
		VALUES ($1, $2, $3, $4, $5, $6, false)`,
		ulid.MustNew(), userID, req.SentenceID, req.Voice,
		len([]rune(text)), time.Since(start).Milliseconds())

	httpx.WriteJSON(w, http.StatusCreated, map[string]any{"audio_id": audioID, "cached": false})
}

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

// ---- GET /audio/{id}/file：校验归属后流式返回音频 ----
// 2026-10-27 改为服务端代理流：原「302 → RustFS 预签名 URL」的跨域跳转在
// 浏览器媒体加载场景不可靠（fetch manual redirect 的 opaqueredirect 不可读、
// Audio 跨域加载受网络层行为影响，见 P-026 续）；单句 MP3 仅 20—30KB，
// 代理流成本可忽略。Audio 元素无法携带 Bearer，前端经 fetch(blob) 播放。

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

	obj, err := h.store.GetObject(r.Context(), objectKey)
	if err != nil {
		httpx.Err(w, httpx.BadGateway("对象存储读取失败: " + err.Error()))
		return
	}
	defer obj.Close()
	w.Header().Set("Content-Type", "audio/mpeg")
	w.Header().Set("Cache-Control", "private, max-age=86400")
	io.Copy(w, obj)
}
