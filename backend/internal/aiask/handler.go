// Package aiask 「问AI」：以生词本句子为基础上下文与 MiniMax M3 对话（最高思考档）。
// POST /api/v1/ai/ask：服务端拼 system 上下文 + 前端历史 + 本次提问，思考内容经
// reasoning_split 走 reasoning_content 单独返回，前端可折叠展示。
package aiask

import (
	"bytes"
	"fmt"
	"encoding/json"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/greatgeek/TingShuo/backend/internal/auth"
	"github.com/greatgeek/TingShuo/backend/internal/httpx"
	"github.com/greatgeek/TingShuo/backend/pkg/ulid"
	"github.com/jackc/pgx/v5/pgxpool"
)

type Handler struct {
	pool   *pgxpool.Pool
	client *http.Client
}

func NewHandler(pool *pgxpool.Pool) *Handler {
	return &Handler{pool: pool, client: &http.Client{Timeout: 300 * time.Second}}
}

type msg struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

const systemTpl = `你是面向中文学习者的日语学习助手。学习者正在研读下面这句日语（含假名注音与中文译文），所属生词一并给出：
句子：%s
注音：%s
中文：%s
生词：%s（%s）%s
回答要求：
1. 用中文回答；引用日语时必须带假名注音；
2. 结合该句讲清语法点、助词、活用、使用场景与易错点，可给 1—2 个同类例句（同样带注音与中文）；
3. 只回答与这句日语或日语学习相关的问题，无关话题礼貌拒绝；
4. 直接回答，不要复述本提示词。`

// Ask POST /api/v1/ai/ask {text, ruby_tokens, translation, word, history, question}
func (h *Handler) Ask(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Text        string          `json:"text"`
		Ruby        json.RawMessage `json:"ruby_tokens"`
		Translation string          `json:"translation"`
		Word        struct {
			Surface string `json:"surface"`
			Reading string `json:"reading"`
			Meaning string `json:"meaning"`
		} `json:"word"`
		History  []msg  `json:"history"`
		Question string `json:"question"`
	}
	if e := httpx.Decode(r, &req); e != nil {
		httpx.Err(w, e)
		return
	}
	req.Question = strings.TrimSpace(req.Question)
	if req.Text == "" || req.Question == "" {
		httpx.Err(w, httpx.BadRequest("EMPTY", "句子与问题都不能为空"))
		return
	}
	if len([]rune(req.Question)) > 500 {
		httpx.Err(w, httpx.BadRequest("TOO_LONG", "问题过长（>500 字）"))
		return
	}
	ruby := strings.TrimSpace(string(req.Ruby))
	if ruby == "" || ruby == "null" {
		ruby = "（無）"
	}
	system := fmt.Sprintf(systemTpl, req.Text, ruby, req.Translation, req.Word.Surface, req.Word.Reading, req.Word.Meaning)

	msgs := []map[string]any{{"role": "system", "content": system}}
	n := len(req.History)
	if n > 20 {
		req.History = req.History[n-20:]
	}
	for _, m := range req.History {
		if (m.Role == "user" || m.Role == "assistant") && m.Content != "" && len([]rune(m.Content)) <= 4000 {
			msgs = append(msgs, map[string]any{"role": m.Role, "content": m.Content})
		}
	}
	msgs = append(msgs, map[string]any{"role": "user", "content": req.Question})

	body := map[string]any{
		"model":            os.Getenv("LLM_MODEL"),
		"messages":         msgs,
		"thinking":         map[string]any{"type": "adaptive"}, // M3 显式开启思考
		"reasoning_effort": "max",                              // M3.1-Flash 生效；M3 忽略（无害透传）
		"reasoning_split":  true,                               // 思考走 reasoning_content，不污染正文
	}
	b, _ := json.Marshal(body)
	target := strings.TrimSuffix(os.Getenv("LLM_BASE_URL"), "/") + "/chat/completions"
	start := time.Now()
	hr, err := http.NewRequestWithContext(r.Context(), http.MethodPost, target, bytes.NewReader(b))
	if err != nil {
		httpx.Err(w, httpx.Internal(err.Error()))
		return
	}
	hr.Header.Set("Content-Type", "application/json")
	hr.Header.Set("Authorization", "Bearer "+os.Getenv("LLM_API_KEY"))
	resp, err := h.client.Do(hr)
	if err != nil {
		httpx.Err(w, httpx.BadGateway("AI 请求失败: "+err.Error()))
		return
	}
	defer resp.Body.Close()
	var out struct {
		Choices []struct {
			Message struct {
				Content   string `json:"content"`
				Reasoning string `json:"reasoning_content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil || resp.StatusCode != http.StatusOK || len(out.Choices) == 0 {
		httpx.Err(w, httpx.BadGateway("AI 上游异常"))
		return
	}
	reply := strings.TrimSpace(out.Choices[0].Message.Content)
	reasoning := strings.TrimSpace(out.Choices[0].Message.Reasoning)
	if reply == "" {
		httpx.Err(w, httpx.BadGateway("AI 返回空回复"))
		return
	}
	userID := auth.FromContext(r.Context())
	_, _ = h.pool.Exec(r.Context(), `
		INSERT INTO llm_call_log (id, user_id, lesson_id, model, prompt_tokens, output_tokens, duration_ms)
		VALUES ($1, $2, NULL, $3, $4, $5, $6)`,
		ulid.MustNew(), userID, os.Getenv("LLM_MODEL"),
		len([]rune(system+req.Question)), len([]rune(reply)), time.Since(start).Milliseconds())

	httpx.WriteJSON(w, http.StatusOK, map[string]any{"reply": reply, "reasoning": reasoning})
}
