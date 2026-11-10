package wordclip

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

const (
	searchTake     = 20
	maxAttempts    = 3
	quotaReserve   = 500 // 月度配额留 500 次缓冲，不在试点/补词时打满
	idleInterval   = 2 * time.Second
	quotaSyncEvery = 50
)

type Worker struct {
	pool *pgxpool.Pool
	cli  *Client

	quotaUsed   int
	quotaLimit  int
	quotaPeriod int
	searches    int
	blockedTill time.Time
}

func NewWorker(pool *pgxpool.Pool) *Worker {
	return &Worker{pool: pool, cli: NewClient(), quotaLimit: 5000}
}

// Start 后台常驻：逐词搜索、翻译并落库。NADE_API_KEY 未配置时不启动。
func (w *Worker) Start(ctx context.Context) {
	if w.cli == nil {
		log.Printf("wordclip: NADE_API_KEY 未配置，原声片段 worker 停用")
		return
	}
	w.syncQuota(ctx)
	go w.loop(ctx)
}

func (w *Worker) loop(ctx context.Context) {
	for {
		if wait := time.Until(w.blockedTill); wait > 0 {
			if wait > 10*time.Minute {
				wait = 10 * time.Minute
			}
			if !sleepCtx(ctx, wait) {
				return
			}
			continue
		}
		if w.quotaLeft() <= 0 {
			w.syncQuota(ctx)
			if w.quotaLeft() <= 0 {
				w.blockedTill = periodEnd(w.quotaPeriod)
				log.Printf("wordclip: 月度配额触底（used=%d limit=%d），暂停到 %s",
					w.quotaUsed, w.quotaLimit, w.blockedTill.Format("2006-01-02"))
				continue
			}
		}

		job, err := w.claim(ctx)
		if errors.Is(err, pgx.ErrNoRows) {
			if !sleepCtx(ctx, idleInterval) {
				return
			}
			continue
		}
		if err != nil {
			log.Printf("wordclip: 取任务失败: %v", err)
			if !sleepCtx(ctx, 5*time.Second) {
				return
			}
			continue
		}

		w.process(ctx, job)
		if !sleepCtx(ctx, 400*time.Millisecond) {
			return
		}
	}
}

type clipJob struct {
	wordKey  string
	surface  string
	reading  string
	meaning  string
	attempts int
}

func (w *Worker) claim(ctx context.Context) (clipJob, error) {
	var j clipJob
	err := w.pool.QueryRow(ctx, `
		WITH next AS (
			SELECT word_key FROM word_clip_queue
			WHERE status = 'pending'
			   OR (status = 'running' AND updated_at < now() - interval '15 minutes')
			ORDER BY created_at
			LIMIT 1
			FOR UPDATE SKIP LOCKED
		)
		UPDATE word_clip_queue q
		SET status = 'running', attempts = q.attempts + 1, updated_at = now()
		FROM next
		WHERE q.word_key = next.word_key
		RETURNING q.word_key, q.surface, q.reading, q.meaning, q.attempts`,
	).Scan(&j.wordKey, &j.surface, &j.reading, &j.meaning, &j.attempts)
	return j, err
}

func (w *Worker) process(ctx context.Context, job clipJob) {
	if skipSurface(job.surface) {
		w.finish(ctx, job.wordKey, "skipped", "词面不适配原声例句")
		return
	}
	if err := w.searchAndStore(ctx, job); err != nil {
		w.onSearchError(ctx, job, err)
	}
}

func (w *Worker) searchAndStore(ctx context.Context, job clipJob) error {
	segs, media, err := w.cli.Search(ctx, job.surface, searchTake)
	w.quotaUsed++
	w.searches++
	if w.searches%quotaSyncEvery == 0 {
		w.syncQuota(ctx)
	}
	if err != nil {
		return err
	}
	cands := pickClips(job.surface, job.reading, segs, media)
	if len(cands) == 0 {
		w.finish(ctx, job.wordKey, "skipped", "未找到含该词的合适台词")
		return nil
	}

	texts, terr := translateClips(ctx, job.surface, job.reading, job.meaning, cands)
	kept := make([]keptClip, 0, maxClips)
	if terr == nil {
		for i, c := range cands {
			t, ok := texts[i]
			if !ok || !t.SenseOK {
				continue
			}
			kept = append(kept, keptClip{c: c, zh: t.ZH, src: "llm"})
		}
	}
	if len(kept) == 0 && terr != nil {
		// LLM 不可用时退回英文：原声与截图仍可做记忆辅助
		for _, c := range cands {
			en := strings.TrimSpace(c.seg.TextEn.Content)
			if en == "" {
				continue
			}
			kept = append(kept, keptClip{c: c, zh: en, src: "en"})
		}
	}
	if len(kept) == 0 {
		msg := "句中没有词单所记的词义"
		if terr != nil {
			msg = "翻译失败且无英文回退"
		}
		w.finish(ctx, job.wordKey, "skipped", msg)
		return nil
	}
	if err := w.store(ctx, job.wordKey, kept); err != nil {
		return err
	}
	w.finish(ctx, job.wordKey, "done", "")
	return nil
}

type keptClip struct {
	c   candidate
	zh  string
	src string
}

func (w *Worker) store(ctx context.Context, wordKey string, kept []keptClip) error {
	tx, err := w.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)

	if _, err := tx.Exec(ctx, `DELETE FROM word_clip WHERE word_key = $1`, wordKey); err != nil {
		return err
	}
	for i, k := range kept {
		tokens, _ := json.Marshal(k.c.tokens)
		mediaName := k.c.mediaName
		if mediaName == "" {
			mediaName = "原声"
		}
		_, err := tx.Exec(ctx, `
			INSERT INTO word_clip
				(word_key, segment_id, text_ja, text_zh, text_en, zh_source, tokens,
				 match_start, match_end, match_text, start_ms, end_ms,
				 media_id, media_name, episode, episode_title,
				 image_url, audio_url, video_url, score, sort_order)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19,$20,$21)
			ON CONFLICT (word_key, segment_id) DO NOTHING`,
			wordKey, k.c.seg.PublicID, k.c.seg.TextJa.Content, k.zh,
			k.c.seg.TextEn.Content, k.src, tokens,
			k.c.matchStart, k.c.matchEnd, k.c.matchText,
			k.c.seg.StartTimeMs, k.c.seg.EndTimeMs,
			k.c.seg.MediaPublicID, mediaName, k.c.seg.Episode, k.c.seg.EpisodeTitle,
			k.c.seg.Urls.ImageURL, k.c.seg.Urls.AudioURL, k.c.seg.Urls.VideoURL,
			k.c.score, i)
		if err != nil {
			return err
		}
	}
	_, err = tx.Exec(ctx, `
		UPDATE word_clip_queue SET status = 'done', last_error = '', updated_at = now()
		WHERE word_key = $1`, wordKey)
	if err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func (w *Worker) onSearchError(ctx context.Context, job clipJob, err error) {
	var apiErr *APIError
	if errors.As(err, &apiErr) {
		switch {
		case apiErr.QuotaExceeded():
			w.syncQuota(ctx)
			w.blockedTill = periodEnd(w.quotaPeriod)
			w.requeue(ctx, job.wordKey, "配额耗尽: "+apiErr.Message)
			log.Printf("wordclip: 配额耗尽，暂停搜索: %v", err)
			return
		case apiErr.RateLimited():
			w.requeue(ctx, job.wordKey, "限流: "+apiErr.Message)
			w.blockedTill = time.Now().Add(5 * time.Second)
			return
		case apiErr.Status == 503 || apiErr.Status >= 500:
			w.requeue(ctx, job.wordKey, "上游不可用: "+apiErr.Message)
			w.blockedTill = time.Now().Add(30 * time.Second)
			return
		}
	}
	if job.attempts >= maxAttempts {
		w.finish(ctx, job.wordKey, "failed", err.Error())
		return
	}
	w.requeue(ctx, job.wordKey, err.Error())
	log.Printf("wordclip: %s 搜索失败（第 %d 次）: %v", job.surface, job.attempts, err)
}

func (w *Worker) requeue(ctx context.Context, key, msg string) {
	_, _ = w.pool.Exec(ctx, `
		UPDATE word_clip_queue SET status = 'pending', last_error = $2, updated_at = now()
		WHERE word_key = $1`, key, truncate(msg, 300))
}

func (w *Worker) finish(ctx context.Context, key, status, msg string) {
	_, _ = w.pool.Exec(ctx, `
		UPDATE word_clip_queue SET status = $2, last_error = $3, updated_at = now()
		WHERE word_key = $1`, key, status, truncate(msg, 300))
}

func (w *Worker) quotaLeft() int {
	if w.quotaLimit <= 0 {
		return 1
	}
	return w.quotaLimit - w.quotaUsed - quotaReserve
}

// syncQuota 用 /v1/user/me 校准本地计数；失败时保留本地估算，不阻塞任务。
func (w *Worker) syncQuota(ctx context.Context) {
	q, err := w.cli.Me(ctx)
	if err != nil {
		return
	}
	if q.Period != 0 && q.Period != w.quotaPeriod {
		if w.quotaPeriod != 0 && q.Used < w.quotaUsed {
			w.quotaUsed = 0
		}
		w.quotaPeriod = q.Period
	}
	if q.Used > w.quotaUsed {
		w.quotaUsed = q.Used
	}
	if q.Limit > 0 {
		w.quotaLimit = q.Limit
	}
}

func periodEnd(period int) time.Time {
	if period < 197001 {
		return time.Now().Add(24 * time.Hour)
	}
	y := period / 100
	m := time.Month(period % 100)
	if m == 12 {
		return time.Date(y+1, 1, 1, 0, 0, 0, 0, time.Local)
	}
	return time.Date(y, m+1, 1, 0, 0, 0, 0, time.Local)
}

func sleepCtx(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

func truncate(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n])
}
