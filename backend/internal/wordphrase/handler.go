// Handler 生词常用词组接口：缓存优先，未命中才请求大模型并落缓存。
package wordphrase

import (
	"net/http"
	"os"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/greatgeek/TingShuo/backend/internal/auth"
	"github.com/greatgeek/TingShuo/backend/internal/httpx"
	"github.com/greatgeek/TingShuo/backend/pkg/llmclient"
	"github.com/greatgeek/TingShuo/backend/pkg/ulid"
)

var (
	phraseLim   = httpx.NewLimiter(5, 10)  // 每用户 5/分钟（仅缓存未命中时计）
	phraseDaily = httpx.NewDailyQuota(150) // 每日 LLM 成本护栏
)

type Handler struct {
	pool   *pgxpool.Pool
	client *llmclient.Client
}

func NewHandler(pool *pgxpool.Pool) *Handler {
	return &Handler{
		pool: pool,
		client: llmclient.New(
			os.Getenv("LLM_BASE_URL"), os.Getenv("LLM_API_KEY"), os.Getenv("LLM_MODEL")),
	}
}

// Get GET /words/{id}/phrases：缓存优先；未命中才生成，已有但不足 5 条时按需补写。
// 补写失败（额度/限速/网络）一律回退返回已有条目，不向用户报错。
func (h *Handler) Get(w http.ResponseWriter, r *http.Request) {
	userID := auth.FromContext(r.Context())
	wordID := chi.URLParam(r, "id")

	var surface string
	var reading, pos, meaning *string
	err := h.pool.QueryRow(r.Context(), `
		SELECT surface, reading, pos, meaning FROM word WHERE id = $1 AND user_id = $2`,
		wordID, userID).Scan(&surface, &reading, &pos, &meaning)
	if err != nil {
		httpx.Err(w, httpx.NotFound("生词不存在或不属于当前用户"))
		return
	}

	cachedItems, cached := Load(r.Context(), h.pool, wordID)
	if cached && len(cachedItems) >= WantCount {
		httpx.WriteJSON(w, http.StatusOK, map[string]any{
			"word_id": wordID, "surface": surface, "items": cachedItems, "cached": true,
		})
		return
	}
	if cached && !h.canGenerate(userID) {
		httpx.WriteJSON(w, http.StatusOK, map[string]any{
			"word_id": wordID, "surface": surface, "items": cachedItems, "cached": true,
		})
		return
	}
	if !cached && (os.Getenv("LLM_API_KEY") == "" || os.Getenv("LLM_BASE_URL") == "") {
		httpx.Err(w, httpx.NewError(http.StatusServiceUnavailable, "LLM_UNAVAILABLE", "词组服务未配置"))
		return
	}
	if !cached && !h.canGenerate(userID) {
		httpx.Err(w, httpx.NewError(http.StatusTooManyRequests, "QUOTA", "词组请求过于频繁，请稍后再试"))
		return
	}

	// 老词条可能没有读音：同词面在库内只有唯一读音时继承，避免词典误读被当词条读音。
	rd := deref(reading)
	if rd == "" {
		var uniq int
		var got string
		_ = h.pool.QueryRow(r.Context(), `
			SELECT min(reading), count(DISTINCT reading) FROM word
			WHERE surface = $1 AND coalesce(reading,'') <> ''`, surface).Scan(&got, &uniq)
		if uniq == 1 {
			rd = got
		}
	}

	info := WordInfo{Surface: surface, Reading: rd, Pos: deref(pos), Meaning: deref(meaning)}
	_ = h.pool.QueryRow(r.Context(), `
		SELECT s.text FROM notebook_sentence s
		JOIN notebook_word nw ON nw.id = s.notebook_word_id
		WHERE nw.word_id = $1
		ORDER BY char_length(s.text) LIMIT 1`, wordID).Scan(&info.ExampleText)

	start := time.Now()
	var items []Item
	var stats GenStats
	if cached {
		start = time.Now()
		res, s, err := Supplement(r.Context(), h.client,
			[]SupplementInput{{Info: info, Existing: cachedItems, Need: WantCount - len(cachedItems)}}, false)
		if err != nil {
			// 额度/限速/网络失败：已有条目照常返回
			httpx.WriteJSON(w, http.StatusOK, map[string]any{
				"word_id": wordID, "surface": surface, "items": cachedItems, "cached": true,
			})
			return
		}
		stats = s
		items = MergeItems(cachedItems, res[0].Items)
	} else {
		res, s, err := Generate(r.Context(), h.client, []WordInfo{info}, false)
		stats = s
		if err != nil {
			httpx.Err(w, httpx.BadGateway("词组生成失败: "+err.Error()))
			return
		}
		items = res[0].Items
		if len(items) < WantCount {
			// 一次重试：模型偶尔给出不含词干或带标点的条目
			res2, s2, err2 := Generate(r.Context(), h.client, []WordInfo{info}, true)
			stats.PromptTokens += s2.PromptTokens
			stats.CompletionTokens += s2.CompletionTokens
			if err2 == nil && len(res2[0].Items) > len(items) {
				items = res2[0].Items
			}
		}
		if len(items) == 0 {
			httpx.Err(w, httpx.BadGateway("词组生成失败：没有合格条目，请稍后再试"))
			return
		}
	}
	if err := Save(r.Context(), h.pool, wordID, items, os.Getenv("LLM_MODEL")); err != nil {
		if !cached {
			httpx.Err(w, httpx.Internal("词组缓存写入失败"))
			return
		}
		items = cachedItems
	}
	_, _ = h.pool.Exec(r.Context(), `
		INSERT INTO llm_call_log (id, user_id, lesson_id, model, prompt_tokens, output_tokens, duration_ms)
		VALUES ($1, $2, NULL, $3, $4, $5, $6)`,
		ulid.MustNew(), userID, os.Getenv("LLM_MODEL"),
		stats.PromptTokens, stats.CompletionTokens, time.Since(start).Milliseconds())

	httpx.WriteJSON(w, http.StatusOK, map[string]any{
		"word_id": wordID, "surface": surface, "items": items, "cached": cached,
	})
}

// canGenerate LLM 已配置且通过限速/日额度检查。
func (h *Handler) canGenerate(userID string) bool {
	if os.Getenv("LLM_API_KEY") == "" || os.Getenv("LLM_BASE_URL") == "" {
		return false
	}
	return phraseLim.Allow(userID) && phraseDaily.Allow(userID)
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}