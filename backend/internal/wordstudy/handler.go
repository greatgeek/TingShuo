// Package wordstudy 实现背词（词级 SRS）：新学 + 到期复习，与句子复习（review 包）
// 并行独立的两条轨道。调度复用 review.NextInterval（1→×2 封顶 30，答错重置 1）。
//
// 生命周期：词单录入 → 生词库累积 → 新学建卡 → SRS 循环 → 掌握（status=mastered）。
// 一词没有 word_review_item 卡 = 新学候选；有卡且未掌握且到期 = 复习候选。
package wordstudy

import (
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/greatgeek/TingShuo/backend/internal/auth"
	"github.com/greatgeek/TingShuo/backend/internal/httpx"
	"github.com/greatgeek/TingShuo/backend/internal/review"
	"github.com/greatgeek/TingShuo/backend/pkg/ttsclient"
	"github.com/greatgeek/TingShuo/backend/pkg/ulid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

const (
	defaultVoice = "Nanami"
	defaultRate  = 1.0
)

type Handler struct {
	pool  *pgxpool.Pool
	azure *ttsclient.Client
}

func NewHandler(pool *pgxpool.Pool, azure *ttsclient.Client) *Handler {
	return &Handler{pool: pool, azure: azure}
}

// Status GET /word-study/status —— 首页背词卡片：新学候选数 + 今日到期数。
func (h *Handler) Status(w http.ResponseWriter, r *http.Request) {
	userID := auth.FromContext(r.Context())
	var fresh, due int
	err := h.pool.QueryRow(r.Context(), `
		SELECT
			(SELECT count(*) FROM word w
			 WHERE w.user_id = $1 AND NOT EXISTS
			   (SELECT 1 FROM word_review_item c WHERE c.word_id = w.id)),
			(SELECT count(*) FROM word_review_item
			 WHERE user_id = $1 AND status = 'learning' AND due_date <= CURRENT_DATE)`,
		userID).Scan(&fresh, &due)
	if err != nil {
		httpx.Err(w, httpx.Internal(err.Error()))
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"fresh": fresh, "due": due})
}

// NewWords GET /word-study/new?limit=20 —— 新学：没有卡的最旧 N 词（先清旧账）。
// 附带每词首个可用例句音频 id（可空），供翻面后直接点播。
func (h *Handler) NewWords(w http.ResponseWriter, r *http.Request) {
	userID := auth.FromContext(r.Context())
	rows, err := h.pool.Query(r.Context(), `
		SELECT w.id, w.surface, w.reading, w.pos, w.meaning,
		       COALESCE(BOOL_OR(lw.is_key), false), aa.audio_id
		FROM word w
		LEFT JOIN lesson_word lw ON lw.word_id = w.id
		LEFT JOIN LATERAL (
			SELECT a.id AS audio_id FROM word_sentence ws
			JOIN audio_asset a ON a.sentence_id = ws.sentence_id AND a.status = 'ready'
			WHERE ws.word_id = w.id
			ORDER BY (a.rate = 1.0) DESC LIMIT 1
		) aa ON true
		WHERE w.user_id = $1 AND NOT EXISTS
			(SELECT 1 FROM word_review_item c WHERE c.word_id = w.id)
		GROUP BY w.id, aa.audio_id
		ORDER BY w.created_at ASC
		LIMIT 20`, userID)
	if err != nil {
		httpx.Err(w, httpx.Internal(err.Error()))
		return
	}
	defer rows.Close()
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"words": scanWordRows(rows), "mode": "new"})
}

// DueWords GET /word-study/due —— 复习：有卡、未掌握、到期的词，按到期时间排序。
func (h *Handler) DueWords(w http.ResponseWriter, r *http.Request) {
	userID := auth.FromContext(r.Context())
	rows, err := h.pool.Query(r.Context(), `
		SELECT w.id, w.surface, w.reading, w.pos, w.meaning,
		       COALESCE(BOOL_OR(lw.is_key), false), aa.audio_id,
		       c.interval_days, c.due_date::text, c.error_count
		FROM word_review_item c
		JOIN word w ON w.id = c.word_id
		LEFT JOIN lesson_word lw ON lw.word_id = w.id
		LEFT JOIN LATERAL (
			SELECT a.id AS audio_id FROM audio_asset a
			WHERE a.sentence_id IN
			   (SELECT sentence_id FROM word_sentence WHERE word_id = w.id)
			  AND a.status = 'ready'
			ORDER BY (a.rate = 1.0) DESC LIMIT 1
		) aa ON true
		WHERE c.user_id = $1 AND c.status = 'learning' AND c.due_date <= CURRENT_DATE
		GROUP BY w.id, aa.audio_id, c.interval_days, c.due_date, c.error_count
		ORDER BY c.due_date, c.error_count DESC`, userID)
	if err != nil {
		httpx.Err(w, httpx.Internal(err.Error()))
		return
	}
	defer rows.Close()
	// 到期列表额外带卡状态（间隔/错次），前端展示用
	type dueWord struct {
		word
		IntervalDays int    `json:"interval_days"`
		DueDate      string `json:"due_date"`
		ErrorCount   int    `json:"error_count"`
	}
	items := []dueWord{}
	for rows.Next() {
		var it dueWord
		if err := rows.Scan(&it.ID, &it.Surface, &it.Reading, &it.Pos, &it.Meaning,
			&it.IsKey, &it.AudioID, &it.IntervalDays, &it.DueDate, &it.ErrorCount); err != nil {
			httpx.Err(w, httpx.Internal(err.Error()))
			return
		}
		items = append(items, it)
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"words": items, "mode": "due"})
}

// Audio GET /word-study/{word_id}/audio —— 单词发音：命中 word_audio 缓存直接返回，
// 未命中合成（词面文本，若带读音注音则用读音保证发音准确）后落库再返回。
func (h *Handler) Audio(w http.ResponseWriter, r *http.Request) {
	userID := auth.FromContext(r.Context())
	wordID := chi.URLParam(r, "word_id")

	var surface string
	var reading *string
	err := h.pool.QueryRow(r.Context(),
		`SELECT surface, reading FROM word WHERE id = $1 AND user_id = $2`,
		wordID, userID).Scan(&surface, &reading)
	if err != nil {
		httpx.Err(w, httpx.NotFound("生词不存在或不属于当前用户"))
		return
	}

	// 命中缓存：直接流式返回
	var cached []byte
	err = h.pool.QueryRow(r.Context(),
		`SELECT data FROM word_audio WHERE word_id = $1`, wordID).Scan(&cached)
	if err == nil {
		w.Header().Set("Content-Type", "audio/mpeg")
		w.Header().Set("Content-Length", fmt.Sprint(len(cached)))
		w.Write(cached)
		return
	}
	if err != pgx.ErrNoRows {
		httpx.Err(w, httpx.Internal(err.Error()))
		return
	}

	// 未命中：合成。读音有注音尾码（如「もじ ①」）时截掉，只用假名
	speakText := surface
	if reading != nil {
		r := strings.TrimSpace(strings.Split(*reading, " ")[0])
		if r != "" {
			speakText = r
		}
	}
	audio, err := h.azure.Synthesize(r.Context(), speakText, defaultVoice, defaultRate)
	if err != nil {
		httpx.Err(w, httpx.BadGateway("TTS 上游失败: "+err.Error()))
		return
	}
	// 落库缓存（并发插入竞态无害：UNIQUE 主键，失败也照常返回音频）
	_, _ = h.pool.Exec(r.Context(),
		`INSERT INTO word_audio (word_id, voice, rate, data) VALUES ($1, $2, $3, $4)
		 ON CONFLICT (word_id) DO NOTHING`,
		wordID, defaultVoice, defaultRate, audio)

	w.Header().Set("Content-Type", "audio/mpeg")
	w.Header().Set("Content-Length", fmt.Sprint(len(audio)))
	w.Write(audio)
}

type word struct {
	ID      string  `json:"id"`
	Surface string  `json:"surface"`
	Reading *string `json:"reading"`
	Pos     *string `json:"pos"`
	Meaning *string `json:"meaning"`
	IsKey   bool    `json:"is_key"`
	AudioID *string `json:"audio_id"` // 首个可用例句音频（可空）
}

func scanWordRows(rows pgx.Rows) []word {
	items := []word{}
	for rows.Next() {
		var it word
		if err := rows.Scan(&it.ID, &it.Surface, &it.Reading, &it.Pos, &it.Meaning,
			&it.IsKey, &it.AudioID); err != nil {
			continue
		}
		items = append(items, it)
	}
	return items
}

// Result POST /word-study/{word_id}/result {result:"pass"|"fail"}
// —— 新学与复习统一入口：无卡则建卡（新学），有卡则按调度更新（复习）。
// 间隔达 30 天封顶且答对 → mastered；答错 → word.mastery 标 weak。
func (h *Handler) Result(w http.ResponseWriter, r *http.Request) {
	userID := auth.FromContext(r.Context())
	wordID := chi.URLParam(r, "word_id")

	var req struct {
		Result review.Result `json:"result"`
	}
	if e := httpx.Decode(r, &req); e != nil {
		httpx.Err(w, e)
		return
	}
	if req.Result != review.Pass && req.Result != review.Fail {
		httpx.Err(w, httpx.BadRequest("RESULT_INVALID", "result 仅支持 pass/fail"))
		return
	}

	// 归属校验
	var exists bool
	if err := h.pool.QueryRow(r.Context(),
		`SELECT EXISTS (SELECT 1 FROM word WHERE id = $1 AND user_id = $2)`,
		wordID, userID).Scan(&exists); err != nil || !exists {
		httpx.Err(w, httpx.NotFound("生词不存在或不属于当前用户"))
		return
	}

	var curInterval int
	err := h.pool.QueryRow(r.Context(),
		`SELECT interval_days FROM word_review_item WHERE word_id = $1`, wordID).
		Scan(&curInterval)
	hasCard := err == nil

	interval, dueDate, errDelta := review.ApplyResult(curInterval, req.Result, time.Now())
	if !hasCard {
		// 新学建卡：首次固定 interval=1、明天到期（与句子复习入池口径一致）；
		// interval_days 记 1，下次答对 ×2=2 天起步
		interval = 1
		dueDate = time.Now().AddDate(0, 0, 1)
	}

	if hasCard {
		mastered := req.Result == review.Pass && interval >= review.MaxIntervalDays
		_, err = h.pool.Exec(r.Context(), `
			UPDATE word_review_item SET
				interval_days = $2, due_date = $3::date, last_result = $4,
				error_count = error_count + $5,
				status = CASE WHEN $6 THEN 'mastered' ELSE status END,
				updated_at = now()
			WHERE word_id = $1`,
			wordID, interval, dueDate.Format("2006-01-02"), string(req.Result),
			errDelta, mastered)
	} else {
		_, err = h.pool.Exec(r.Context(), `
			INSERT INTO word_review_item
				(id, user_id, word_id, interval_days, due_date, last_result, error_count)
			VALUES ($1, $2, $3, $4, $5::date, $6, $7)`,
			ulid.MustNew(), userID, wordID, interval,
			dueDate.Format("2006-01-02"), string(req.Result), errDelta)
	}
	if err != nil {
		httpx.Err(w, httpx.Internal("更新背词状态失败: "+err.Error()))
		return
	}

	// mastery 联动：答错标 weak、掌握标 learned，其余保持不变
	// （'key' = 重点词是词单语义，背词轨道不碰）
	var mastery string
	if req.Result == review.Fail {
		mastery = "weak"
	} else if hasCard && interval >= review.MaxIntervalDays {
		mastery = "learned"
	}
	if mastery != "" {
		if _, err := h.pool.Exec(r.Context(),
			`UPDATE word SET mastery = $2, updated_at = now() WHERE id = $1`,
			wordID, mastery); err != nil {
			httpx.Err(w, httpx.Internal(err.Error()))
			return
		}
	}

	httpx.WriteJSON(w, http.StatusOK, map[string]any{
		"word_id": wordID, "interval_days": interval,
		"due_date": dueDate.Format("2006-01-02"), "result": string(req.Result),
	})
}
