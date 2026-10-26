// Package dev 提供仅限本地开发的浏览器试用支持。
//
// 为什么存在：生产架构是 BYOK——Web 客户端在浏览器里直连 LLM/Azure（FR-3.12）。
// 浏览器直连的 CORS 可行性属硬风险（部署文档 §6.4），开发期用本代理绕过，
// Key 仍只来自 .env.dev（冒烟调试用途，部署文档 §2.3），服务端不落库、不记日志。
//
// LLM 生成采用「后台任务 + 轮询」而非同步透传：推理模型生成可达 3—8 分钟，
// 浏览器长连接会被网络层掐断（Failed to fetch）；后台任务使刷新页面、
// 网络抖动都不丢进度。生产部署必须设置 DEV_PROXY=false 关闭本包全部端点。
package dev

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/greatgeek/TingShuo/backend/internal/httpx"
	"github.com/greatgeek/TingShuo/backend/pkg/storage"
	"github.com/greatgeek/TingShuo/backend/pkg/ttsclient"
	"github.com/greatgeek/TingShuo/backend/pkg/ulid"
)

// Enabled 报告开发代理是否开启（默认开，生产显式 DEV_PROXY=false）。
func Enabled() bool { return os.Getenv("DEV_PROXY") != "false" }

// Cors 是本地开发 CORS 中间件（试用页与 API 同源时其实不触发，保留给分离开发场景）。
func Cors(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Authorization, X-LLM-Api-Key, X-Azure-Key")
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PUT, OPTIONS")
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}

type Handler struct {
	azure   *ttsclient.Client
	llmHTTP *http.Client
	store   *storage.Client
}

func NewHandler() (*Handler, error) {
	store, err := storage.New(
		os.Getenv("S3_ENDPOINT"), os.Getenv("S3_ACCESS_KEY"), os.Getenv("S3_SECRET_KEY"),
		os.Getenv("S3_BUCKET_AUDIO"), os.Getenv("S3_BUCKET_RECORDING"))
	if err != nil {
		return nil, err
	}
	return &Handler{
		azure:   ttsclient.New(os.Getenv("AZURE_SPEECH_KEY"), os.Getenv("AZURE_SPEECH_REGION")),
		llmHTTP: &http.Client{Timeout: 600 * time.Second}, // 推理模型全量生成可达数分钟
		store:   store,
	}, nil
}

// Config 返回试用页默认配置（含 .env.dev 的 Key，仅供本地页面预填）。
func (h *Handler) Config(w http.ResponseWriter, r *http.Request) {
	httpx.WriteJSON(w, http.StatusOK, map[string]string{
		"llm_base_url": os.Getenv("LLM_BASE_URL"),
		"llm_model":    os.Getenv("LLM_MODEL"),
		"llm_api_key":  os.Getenv("LLM_API_KEY"),
		"azure_region": os.Getenv("AZURE_SPEECH_REGION"),
		"azure_key":    os.Getenv("AZURE_SPEECH_KEY"),
	})
}

// ---------- LLM 后台任务 ----------

type llmTask struct {
	mu      sync.Mutex
	buf     bytes.Buffer // 累计输出（含思考文本，前端展示进度用）
	done    bool
	errMsg  string
	started time.Time
}

func (t *llmTask) append(s string) { t.mu.Lock(); t.buf.WriteString(s); t.mu.Unlock() }
func (t *llmTask) finish()         { t.mu.Lock(); t.done = true; t.mu.Unlock() }
func (t *llmTask) setErr(msg string) {
	t.mu.Lock(); t.errMsg = msg; t.done = true; t.mu.Unlock()
}
func (t *llmTask) snapshot() (text string, done bool, errMsg string, elapsed time.Duration) {
	t.mu.Lock(); defer t.mu.Unlock()
	return t.buf.String(), t.done, t.errMsg, time.Since(t.started)
}

// tasks 内存任务表（开发工具：进程重启即清空，不做持久化与 GC）。
var tasks sync.Map

// StartLLM POST /dev/llm/start：立即返回 task_id，后台流式完成生成。
func (h *Handler) StartLLM(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Model    string `json:"model"`
		Messages []map[string]string `json:"messages"`
	}
	if e := httpx.Decode(r, &req); e != nil {
		httpx.Err(w, e)
		return
	}
	if req.Model == "" || len(req.Messages) == 0 {
		httpx.Err(w, httpx.BadRequest("PARAMS_INVALID", "model 与 messages 不能为空"))
		return
	}

	id := ulid.MustNew()
	t := &llmTask{started: time.Now()}
	tasks.Store(id, t)

	body, _ := json.Marshal(map[string]any{
		"model": req.Model, "messages": req.Messages, "stream": true,
	})
	target := strings.TrimSuffix(os.Getenv("LLM_BASE_URL"), "/") + "/chat/completions"

	go func() {
		httpReq, err := http.NewRequestWithContext(context.Background(), http.MethodPost, target, bytes.NewReader(body))
		if err != nil {
			t.setErr(err.Error())
			return
		}
		httpReq.Header.Set("Authorization", "Bearer "+os.Getenv("LLM_API_KEY"))
		httpReq.Header.Set("Content-Type", "application/json")
		resp, err := h.llmHTTP.Do(httpReq)
		if err != nil {
			t.setErr("上游请求失败: " + err.Error())
			return
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			raw, _ := io.ReadAll(io.LimitReader(resp.Body, 300))
			t.setErr(fmt.Sprintf("上游 HTTP %d: %s", resp.StatusCode, raw))
			return
		}
		// 解析 SSE：data: {...} 的 delta.content 累计
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
		if err := scanner.Err(); err != nil {
			t.setErr("读取上游中断: " + err.Error())
			return
		}
		t.finish()
	}()

	httpx.WriteJSON(w, http.StatusCreated, map[string]string{"task_id": id})
}

// PollLLM GET /dev/llm/poll?id=：返回累计文本与状态（前端 1s 轮询）。
// 附带退化输出检测：模型偶发陷入重复 token 循环（如整片 "UxUxUx"），
// 轮询时发现尾部字符多样性过低即提前标记失败，让用户立刻重试而非空等。
func (h *Handler) PollLLM(w http.ResponseWriter, r *http.Request) {
	v, ok := tasks.Load(r.URL.Query().Get("id"))
	if !ok {
		httpx.Err(w, httpx.NotFound("任务不存在（服务可能已重启，请重新提交）"))
		return
	}
	t := v.(*llmTask)
	text, done, errMsg, elapsed := t.snapshot()

	// 退化检测：未完成 + 已输出 >1000 字符 + 尾部 200 字符唯一字符数 ≤4 → 判定循环
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
		"elapsed_ms": elapsed.Milliseconds(), "chars": len([]rune(text)),
	})
}

// Upload POST /dev/upload?cache_key=：开发期浏览器上传代理。
// RustFS 未配置 CORS，浏览器直传预签名 PUT 被拦（P-026）；body 为音频字节。
// 生产走 OSS（配置 CORS 后浏览器预签名直传），此端点随 DEV_PROXY=false 关闭。
func (h *Handler) Upload(w http.ResponseWriter, r *http.Request) {
	key := r.URL.Query().Get("cache_key")
	ok, _ := regexp.MatchString(`^[0-9a-f]{64}$`, key)
	if !ok {
		httpx.Err(w, httpx.BadRequest("CACHE_KEY_INVALID", "cache_key 必须为 64 位十六进制"))
		return
	}
	data, err := io.ReadAll(io.LimitReader(r.Body, 10<<20)) // 单句 MP3 上限 10MB
	if err != nil || len(data) == 0 {
		httpx.Err(w, httpx.BadRequest("BODY_INVALID", "音频内容为空"))
		return
	}
	if err := h.store.PutAudio(r.Context(), key, data); err != nil {
		httpx.Err(w, httpx.BadGateway("对象存储上传失败: " + err.Error()))
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// TTSProxy body {text, voice, rate} → SSML → Azure → 直接回传 MP3 字节流。
func (h *Handler) TTSProxy(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Text  string  `json:"text"`
		Voice string  `json:"voice"`
		Rate  float64 `json:"rate"`
	}
	if e := httpx.Decode(r, &req); e != nil {
		httpx.Err(w, e)
		return
	}
	if strings.TrimSpace(req.Text) == "" {
		httpx.Err(w, httpx.BadRequest("TEXT_EMPTY", "text 不能为空"))
		return
	}
	if req.Rate == 0 {
		req.Rate = 1.0
	}
	audio, err := h.azure.Synthesize(r.Context(), req.Text, req.Voice, req.Rate)
	if err != nil {
		httpx.Err(w, httpx.BadGateway("TTS 上游失败: " + err.Error()))
		return
	}
	w.Header().Set("Content-Type", "audio/mpeg")
	w.Header().Set("Content-Length", fmt.Sprint(len(audio)))
	w.Write(audio)
}
