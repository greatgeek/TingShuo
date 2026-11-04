package podcast

import (
	"context"
	"fmt"
	"math"
	"net/http"
	"sync"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/greatgeek/TingShuo/backend/internal/auth"
	"github.com/greatgeek/TingShuo/backend/internal/httpx"
	"github.com/greatgeek/TingShuo/backend/pkg/ulid"
)

// 后台生成任务（v0.4.0 长片）：一集要跑十几到几十分钟，浏览器挂着 SSE 等不现实
// （手机锁屏/切后台连接就断），所以生成落 job 表由服务端 worker 跑，前端轮询进度；
// 关掉页面再回来，按源（目录/课）还能查到同一个 job。
// 与 internal/generate 的 generation_job 同调：重启时诚实标记中断，不假装还在跑。

// Job 为一次生成的进度快照（stage 是人话，前端直接显示）。
type Job struct {
	ID        string `json:"id"`
	Status    string `json:"status"`
	Stage     string `json:"stage"`
	Words     int    `json:"words"`
	Seg       int    `json:"seg"`
	Segs      int    `json:"segs"`
	Round     int    `json:"round"`
	Rounds    int    `json:"rounds"`
	EpisodeID string `json:"episode_id,omitempty"`
	Error     string `json:"error,omitempty"`
	EstMin    int    `json:"est_minutes"`
}

const (
	JobQueued  = "queued"
	JobWriting = "writing"
	JobSynth   = "synthesizing"
	JobDone    = "done"
	JobError   = "error"
)

// minPerWord 每词成片分钟数；yuanPerMin 每分钟成片成本。
// 2026-11-04 实测（12 词 → 171 行 → 11.2 分钟、4300 字、生成 4 分 19 秒）：
// 0.93 分钟/词；成本按火山 token 单价折算（输出音频 100 元/百万 token、1 秒≈25 token，
// 加输入文本 120 元/百万 token）≈ 0.2 元/分钟，以账单为准。
const (
	minPerWord = 0.95
	yuanPerMin = 0.2
)

func estMinutes(words int) int {
	return int(math.Round(float64(words) * minPerWord))
}

// StartWorker 启动后台生成循环（单并发：火山有并发限流，长片也吃内存）。
// 服务重启时把上次进程正在跑的任务标记为中断——合成没有断点续传，重跑要重新烧钱，
// 让用户自己决定比默默重跑诚实。
func (h *Handler) StartWorker(ctx context.Context) {
	h.pool.Exec(ctx, `
		UPDATE podcast_job SET status=$1, error='服务重启中断，请重新生成', stage='已中断', updated_at=now()
		WHERE status IN ($2,$3)`, JobError, JobWriting, JobSynth)
	go func() {
		t := time.NewTicker(2 * time.Second)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				h.runNext(ctx)
			}
		}
	}()
}

// runNext 认领一个排队任务并跑完（FOR UPDATE SKIP LOCKED 保证多实例也不会重复认领）。
func (h *Handler) runNext(ctx context.Context) {
	var (
		id, userID, lessonID, dirID string
	)
	err := h.pool.QueryRow(ctx, `
		UPDATE podcast_job SET status=$1, stage='开始生成', updated_at=now()
		WHERE id = (SELECT id FROM podcast_job WHERE status=$2 ORDER BY created_at LIMIT 1 FOR UPDATE SKIP LOCKED)
		RETURNING id, user_id, coalesce(lesson_id,''), coalesce(dir_id,'')`,
		JobWriting, JobQueued).Scan(&id, &userID, &lessonID, &dirID)
	if err != nil {
		return
	}
	defer func() {
		if p := recover(); p != nil {
			h.finish(ctx, id, "", fmt.Sprintf("内部错误: %v", p))
		}
	}()

	jctx, cancel := context.WithTimeout(ctx, 2*time.Hour)
	defer cancel()
	meta, err := h.generate(jctx, userID, lessonID, dirID, h.jobSink(jctx, id))
	if err != nil {
		h.finish(ctx, id, "", err.Error())
		return
	}
	h.finish(ctx, id, meta.EpisodeID, "")
}

// finish 写终态。
func (h *Handler) finish(ctx context.Context, id, episodeID, errMsg string) {
	status, stage := JobDone, "完成"
	if errMsg != "" {
		status, stage = JobError, "生成失败"
	}
	h.pool.Exec(ctx, `
		UPDATE podcast_job SET status=$1, stage=$2, seg=segs, round=rounds,
			episode_id=NULLIF($3,''), error=NULLIF($4,''), updated_at=now()
		WHERE id=$5`, status, stage, episodeID, errMsg, id)
}

// jobSink 把生成进度写回 job 行（1 秒节流：合成回调按轮触发，很密）。
func (h *Handler) jobSink(ctx context.Context, jobID string) func(Prog) {
	var (
		mu   sync.Mutex
		last time.Time
	)
	return func(p Prog) {
		mu.Lock()
		defer mu.Unlock()
		if time.Since(last) < time.Second {
			return
		}
		last = time.Now()
		h.pool.Exec(ctx, `
			UPDATE podcast_job SET status=$1, stage=$2, seg=$3, segs=$4, round=$5, rounds=$6, updated_at=now()
			WHERE id=$7`, p.Status, p.Stage, p.Seg, p.Segs, p.Round, p.Rounds, jobID)
	}
}

// CreateJob POST /podcast/jobs {lesson_id|dir_id}：排队生成一集，立即返回 job。
// 同源已有进行中的任务时直接返回它——重复点按不会重复烧钱。
func (h *Handler) CreateJob(w http.ResponseWriter, r *http.Request) {
	userID := auth.FromContext(r.Context())
	if !genLim.Allow(userID) || !genDaily.Allow(userID) {
		httpx.Err(w, httpx.NewError(http.StatusTooManyRequests, "QUOTA", "播客生成过于频繁（每日上限 20 集）"))
		return
	}
	var req struct {
		LessonID string `json:"lesson_id"`
		DirID    string `json:"dir_id"`
	}
	if e := httpx.Decode(r, &req); e != nil {
		httpx.Err(w, e)
		return
	}
	in, err := h.loadInput(r.Context(), userID, req.LessonID, req.DirID)
	if err != nil {
		httpx.Err(w, httpx.NotFound(err.Error()))
		return
	}
	if len(in.Words) == 0 {
		httpx.Err(w, httpx.BadRequest("SOURCE_EMPTY", "该来源没有可用于播客的词"))
		return
	}

	if j, ok := h.openJob(r.Context(), userID, req.LessonID, req.DirID); ok {
		httpx.WriteJSON(w, http.StatusOK, j)
		return
	}
	id := ulid.MustNew()
	if _, err := h.pool.Exec(r.Context(), `
		INSERT INTO podcast_job (id, user_id, lesson_id, dir_id, status, stage, words)
		VALUES ($1,$2,NULLIF($3,''),NULLIF($4,''),$5,'排队中',$6)`,
		id, userID, req.LessonID, req.DirID, JobQueued, len(in.Words)); err != nil {
		httpx.Err(w, httpx.BadGateway("排队失败: "+err.Error()))
		return
	}
	httpx.WriteJSON(w, http.StatusAccepted, Job{
		ID: id, Status: JobQueued, Stage: "排队中", Words: len(in.Words), EstMin: estMinutes(len(in.Words)),
	})
}

// openJob 取该源最近一个未结束的 job（幂等返回用）。
func (h *Handler) openJob(ctx context.Context, userID, lessonID, dirID string) (Job, bool) {
	var j Job
	err := h.pool.QueryRow(ctx, `
		SELECT id, status, stage, words, seg, segs, round, rounds FROM podcast_job
		WHERE user_id=$1 AND status IN ($2,$3,$4)
			AND coalesce(dir_id,'')=$5 AND coalesce(lesson_id,'')=$6
		ORDER BY created_at DESC LIMIT 1`,
		userID, JobQueued, JobWriting, JobSynth, dirID, lessonID).
		Scan(&j.ID, &j.Status, &j.Stage, &j.Words, &j.Seg, &j.Segs, &j.Round, &j.Rounds)
	if err != nil {
		return j, false
	}
	j.EstMin = estMinutes(j.Words)
	return j, true
}

// GetJob GET /podcast/jobs/{id}：轮询单个任务进度（归属校验）。
func (h *Handler) GetJob(w http.ResponseWriter, r *http.Request) {
	userID := auth.FromContext(r.Context())
	j, aerr := h.jobByID(r.Context(), chi.URLParam(r, "id"), userID)
	if aerr != nil {
		httpx.Err(w, aerr)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, j)
}

// LatestJob GET /podcast/jobs/latest?dir_id=|lesson_id=：该源最近一个任务（含终态）。
// 前端打开面板时调用，用来恢复"关页面期间已经在跑/已经跑完"的那一集。
func (h *Handler) LatestJob(w http.ResponseWriter, r *http.Request) {
	userID := auth.FromContext(r.Context())
	q := r.URL.Query()
	dirID, lessonID := q.Get("dir_id"), q.Get("lesson_id")
	if dirID == "" && lessonID == "" {
		httpx.Err(w, httpx.BadRequest("PARAMS_INVALID", "dir_id / lesson_id 至少给一个"))
		return
	}
	j, aerr := h.jobBySource(r.Context(), userID, dirID, lessonID)
	if aerr != nil {
		httpx.Err(w, aerr)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, j)
}

const jobCols = `id, status, stage, words, seg, segs, round, rounds, coalesce(episode_id,''), coalesce(error,'')`

func scanJob(row interface {
	Scan(...any) error
}) (Job, error) {
	var j Job
	err := row.Scan(&j.ID, &j.Status, &j.Stage, &j.Words, &j.Seg, &j.Segs, &j.Round, &j.Rounds, &j.EpisodeID, &j.Error)
	j.EstMin = estMinutes(j.Words)
	return j, err
}

func (h *Handler) jobByID(ctx context.Context, id, userID string) (Job, *httpx.APIError) {
	j, err := scanJob(h.pool.QueryRow(ctx,
		`SELECT `+jobCols+` FROM podcast_job WHERE id=$1 AND user_id=$2`, id, userID))
	if err != nil {
		return j, httpx.NotFound("没有该播客任务")
	}
	return j, nil
}

func (h *Handler) jobBySource(ctx context.Context, userID, dirID, lessonID string) (Job, *httpx.APIError) {
	j, err := scanJob(h.pool.QueryRow(ctx, `SELECT `+jobCols+` FROM podcast_job
		WHERE user_id=$1 AND coalesce(dir_id,'')=$2 AND coalesce(lesson_id,'')=$3
		ORDER BY created_at DESC LIMIT 1`, userID, dirID, lessonID))
	if err != nil {
		return j, httpx.NotFound("没有该播客任务")
	}
	return j, nil
}

// Estimate GET /podcast/estimate?dir_id=|lesson_id=：生成前的词数与时长/成本预估（确认框用）。
func (h *Handler) Estimate(w http.ResponseWriter, r *http.Request) {
	userID := auth.FromContext(r.Context())
	q := r.URL.Query()
	in, err := h.loadInput(r.Context(), userID, q.Get("lesson_id"), q.Get("dir_id"))
	if err != nil {
		httpx.Err(w, httpx.NotFound(err.Error()))
		return
	}
	sents := 0
	for _, w := range in.Words {
		sents += len(w.Sentences)
	}
	mins := estMinutes(len(in.Words))
	httpx.WriteJSON(w, http.StatusOK, map[string]any{
		"title": in.Title, "words": len(in.Words), "sentences": sents,
		"est_minutes": mins,
		"est_yuan":    math.Round(float64(mins)*yuanPerMin*10) / 10,
		"segs":        len(planSegments(in.Words)),
	})
}
