// 生成任务服务端化（部署文档 §4 决策）：LLM 材料生成由服务端持 Key 代生成，
// 任务状态落 generation_job 表——服务重启/发布期间前端轮询不 404，可读到终态与已产出文本。
//
// 与 dev 包内存版的区别：dev 是本地试用代理（DEV_PROXY 关闭即消失）；本实现是生产主线，
// 带每用户每日频控（防 Key 盗刷）与 llm_call_log 成本观测。
package generate

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/greatgeek/TingShuo/backend/internal/auth"
	"github.com/greatgeek/TingShuo/backend/internal/httpx"
	"github.com/greatgeek/TingShuo/backend/pkg/ulid"
	"github.com/jackc/pgx/v5/pgxpool"
)

// JobHandler 承载 /api/v1/generate/* 与 /api/v1/config。
type JobHandler struct {
	pool    *pgxpool.Pool
	llmHTTP *http.Client
}

func NewJobHandler(pool *pgxpool.Pool) *JobHandler {
	return &JobHandler{
		pool:    pool,
		llmHTTP: &http.Client{Timeout: 600 * time.Second}, // 推理模型全量生成可达数分钟
	}
}

// dailyLimit 每用户每日材料生成次数上限（防 Key 盗刷，部署文档 §4 安全要点）。
func dailyLimit() int {
	if n, err := strconv.Atoi(os.Getenv("GEN_DAILY_LIMIT")); err == nil && n > 0 {
		return n
	}
	return 10
}

// ---------- 流式任务（内存缓冲 + 定期落库）----------

type jobTask struct {
	mu      sync.Mutex
	buf     bytes.Buffer
	done    bool
	errMsg  string
	started time.Time
}

func (t *jobTask) append(s string) { t.mu.Lock(); t.buf.WriteString(s); t.mu.Unlock() }
func (t *jobTask) finish()         { t.mu.Lock(); t.done = true; t.mu.Unlock() }
func (t *jobTask) setErr(m string) { t.mu.Lock(); t.errMsg = m; t.done = true; t.mu.Unlock() }
func (t *jobTask) snapshot() (text string, done bool, errMsg string, elapsed time.Duration) {
	t.mu.Lock(); defer t.mu.Unlock()
	return t.buf.String(), t.done, t.errMsg, time.Since(t.started)
}

// liveJobs 进行中的任务缓冲（终态后仍保留短暂供轮询读取，进程重启则由 DB 兜底）。
var liveJobs sync.Map

// CleanupStale 启动时把上次进程残留的 running/pending 任务置 failed。
// 上游 SSE 连接随进程死亡无法恢复，诚实标记中断比假装还在跑更好（前端据此提示重试）。
func CleanupStale(ctx context.Context, pool *pgxpool.Pool) {
	_, _ = pool.Exec(ctx, `
		UPDATE generation_job SET status = 'failed', error = '服务重启中断，请重新生成', updated_at = now()
		WHERE status IN ('pending', 'running')`)
}

// Start POST /api/v1/generate/start {lesson_id, messages} → {task_id}。
// model 由服务端 env 决定（Key 与模型不下放客户端）；messages 为对话上下文。
func (h *JobHandler) Start(w http.ResponseWriter, r *http.Request) {
	userID := auth.FromContext(r.Context())
	var req struct {
		LessonID string              `json:"lesson_id"`
		Messages []map[string]string `json:"messages"`
	}
	if e := httpx.Decode(r, &req); e != nil {
		httpx.Err(w, e)
		return
	}
	if req.LessonID == "" || len(req.Messages) == 0 {
		httpx.Err(w, httpx.BadRequest("PARAMS_INVALID", "lesson_id 与 messages 不能为空"))
		return
	}

	// 课程归属校验
	var owner string
	if err := h.pool.QueryRow(r.Context(),
		`SELECT user_id FROM daily_lesson WHERE id = $1`, req.LessonID).Scan(&owner); err != nil {
		httpx.Err(w, httpx.NotFound("课程不存在"))
		return
	}
	if owner != userID {
		httpx.Err(w, httpx.Forbidden("课程不属于当前用户"))
		return
	}

	// 频控：当日该用户材料生成次数
	var today int
	if err := h.pool.QueryRow(r.Context(), `
		SELECT count(*) FROM generation_job j
		JOIN daily_lesson l ON l.id = j.lesson_id
		WHERE l.user_id = $1 AND j.type = 'material' AND j.created_at >= CURRENT_DATE`,
		userID).Scan(&today); err == nil && today >= dailyLimit() {
		httpx.Err(w, httpx.NewError(429, "RATE_LIMITED",
			fmt.Sprintf("今日生成次数已达上限（%d 次/天），明天再来", dailyLimit())))
		return
	}

	model := os.Getenv("LLM_MODEL")
	jobID := ulid.MustNew()
	if _, err := h.pool.Exec(r.Context(), `
		INSERT INTO generation_job (id, lesson_id, type, status, attempts)
		VALUES ($1, $2, 'material', 'running', 1)`,
		jobID, req.LessonID); err != nil {
		httpx.Err(w, httpx.Internal("创建生成任务失败: "+err.Error()))
		return
	}

	t := &jobTask{started: time.Now()}
	liveJobs.Store(jobID, t)

	body, _ := json.Marshal(map[string]any{
		"model": model, "messages": req.Messages, "stream": true,
	})
	target := strings.TrimSuffix(os.Getenv("LLM_BASE_URL"), "/") + "/chat/completions"

	go h.run(jobID, userID, req.LessonID, model, target, body, t)

	httpx.WriteJSON(w, http.StatusCreated, map[string]string{"task_id": jobID})
}

// run 后台流式拉取上游，边收边定期落库 output，终态写 status/error 与 llm_call_log。
func (h *JobHandler) run(jobID, userID, lessonID, model, target string, body []byte, t *jobTask) {
	persist := func(status, errMsg string) {
		text, _, _, _ := t.snapshot()
		_, _ = h.pool.Exec(context.Background(), `
			UPDATE generation_job SET status = $2, error = $3, output = $4, updated_at = now()
			WHERE id = $1`, jobID, status, errMsg, text)
	}

	httpReq, err := http.NewRequestWithContext(context.Background(), http.MethodPost, target, bytes.NewReader(body))
	if err != nil {
		t.setErr(err.Error())
		persist("failed", err.Error())
		return
	}
	httpReq.Header.Set("Authorization", "Bearer "+os.Getenv("LLM_API_KEY"))
	httpReq.Header.Set("Content-Type", "application/json")

	resp, err := h.llmHTTP.Do(httpReq)
	if err != nil {
		t.setErr("上游请求失败: " + err.Error())
		persist("failed", "上游请求失败: "+err.Error())
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 300))
		msg := fmt.Sprintf("上游 HTTP %d: %s", resp.StatusCode, raw)
		t.setErr(msg)
		persist("failed", msg)
		return
	}

	// 定期落库（2s），重启后仍能取回已产出部分
	stop := make(chan struct{})
	go func() {
		tick := time.NewTicker(2 * time.Second)
		defer tick.Stop()
		for {
			select {
			case <-tick.C:
				persist("running", "")
			case <-stop:
				return
			}
		}
	}()

	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 1<<20), 1<<20)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if data == "[DONE]" {
			break
		}
		var j struct {
			Choices []struct {
				Delta struct {
					Content string `json:"content"`
				} `json:"delta"`
			} `json:"choices"`
		}
		if json.Unmarshal([]byte(data), &j) == nil && len(j.Choices) > 0 && j.Choices[0].Delta.Content != "" {
			t.append(j.Choices[0].Delta.Content)
		}
	}
	close(stop)

	if err := scanner.Err(); err != nil {
		t.setErr("读取上游中断: " + err.Error())
		persist("failed", "读取上游中断: "+err.Error())
		h.logCall(userID, lessonID, model, t)
		return
	}
	t.finish()
	text, _, _, _ := t.snapshot()
	if strings.TrimSpace(text) == "" {
		t.setErr("上游返回为空")
		persist("failed", "上游返回为空")
	} else {
		persist("succeeded", "")
	}
	h.logCall(userID, lessonID, model, t)
}

// logCall 成本观测（部署文档 §9.3）：token 数用字符数近似（流式无 usage 回传）。
func (h *JobHandler) logCall(userID, lessonID, model string, t *jobTask) {
	text, _, _, elapsed := t.snapshot()
	_, _ = h.pool.Exec(context.Background(), `
		INSERT INTO llm_call_log (id, user_id, lesson_id, model, prompt_tokens, output_tokens, duration_ms)
		VALUES ($1, $2, $3, $4, 0, $5, $6)`,
		ulid.MustNew(), userID, lessonID, model, len([]rune(text)), elapsed.Milliseconds())
}

// Poll GET /api/v1/generate/poll?id=：内存优先（实时），miss 则读 DB（跨重启）。
func (h *JobHandler) Poll(w http.ResponseWriter, r *http.Request) {
	userID := auth.FromContext(r.Context())
	id := r.URL.Query().Get("id")

	if v, ok := liveJobs.Load(id); ok {
		t := v.(*jobTask)
		text, done, errMsg, elapsed := t.snapshot()
		// 退化检测：尾部字符多样性过低 = 重复循环，提前判失败让用户重试
		runes := []rune(text)
		if !done && errMsg == "" && len(runes) > 1000 {
			tail := runes[len(runes)-200:]
			uniq := map[rune]bool{}
			for _, c := range tail {
				uniq[c] = true
			}
			if len(uniq) <= 4 {
				t.setErr("模型输出退化（重复字符循环），请点「重新生成」重试一次")
				text, done, errMsg, elapsed = t.snapshot()
			}
		}
		if done && errMsg == "" && strings.TrimSpace(text) == "" {
			errMsg = "上游返回为空"
		}
		httpx.WriteJSON(w, http.StatusOK, map[string]any{
			"done": done, "err": errMsg, "text": text,
			"elapsed_ms": elapsed.Milliseconds(), "chars": len(runes),
		})
		return
	}

	// DB 兜底（进程重启后 / 非本进程任务）
	var status, errMsg string
	var output *string
	var createdAt time.Time
	err := h.pool.QueryRow(r.Context(), `
		SELECT j.status, j.error, j.output, j.created_at
		FROM generation_job j JOIN daily_lesson l ON l.id = j.lesson_id
		WHERE j.id = $1 AND l.user_id = $2`, id, userID).
		Scan(&status, &errMsg, &output, &createdAt)
	if err != nil {
		httpx.Err(w, httpx.NotFound("任务不存在"))
		return
	}
	text := ""
	if output != nil {
		text = *output
	}
	done := status == "succeeded" || status == "failed"
	httpx.WriteJSON(w, http.StatusOK, map[string]any{
		"done": done, "err": errMsg, "text": text,
		"elapsed_ms": time.Since(createdAt).Milliseconds(), "chars": len([]rune(text)),
	})
}

// Config GET /api/v1/config：仅暴露模型名（Key 永不下放客户端）。
func (h *JobHandler) Config(w http.ResponseWriter, r *http.Request) {
	httpx.WriteJSON(w, http.StatusOK, map[string]string{
		"llm_model": os.Getenv("LLM_MODEL"),
	})
}
