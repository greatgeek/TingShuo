// Package aiask 「问AI」：以生词本句子或语法知识点为基础上下文与 MiniMax M3 对话（最高思考档）。
// POST /api/v1/ai/ask：服务端拼 system 上下文 + 前端历史 + 本次提问，思考内容经
// reasoning_split 走 reasoning_content 单独返回，前端可折叠展示。
package aiask

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/greatgeek/TingShuo/backend/internal/auth"
	"github.com/greatgeek/TingShuo/backend/internal/httpx"
	"github.com/greatgeek/TingShuo/backend/pkg/ulid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

var (
	askLim   = httpx.NewLimiter(6, 10)  // 每用户 6/分钟
	askDaily = httpx.NewDailyQuota(100) // 每用户 100/天（LLM 成本护栏）
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

const sentenceTpl = `你是面向中文学习者的日语学习助手。学习者正在研读下面这句日语（含假名注音与中文译文）%s：
句子：%s
注音：%s
中文：%s
%s回答要求：
1. 用中文回答；引用日语时必须带假名注音；
2. 结合该句讲清语法点、助词、活用、使用场景与易错点，可给 1—2 个同类例句（同样带注音与中文）；
3. 只回答与这句日语或日语学习相关的问题，无关话题礼貌拒绝；
4. 直接回答，不要复述本提示词。`

// sentenceSystem 语法例句没有所属生词时，不输出空的“生词：”行。
func sentenceSystem(text, ruby, translation, surface, reading, meaning string) string {
	intro, wordLine := "", ""
	if surface != "" {
		intro = "，所属生词一并给出"
		wordLine = fmt.Sprintf("生词：%s（%s）%s\n", surface, reading, meaning)
	}
	return fmt.Sprintf(sentenceTpl, intro, text, ruby, translation, wordLine)
}

type grammarCtx struct {
	Book        string
	LessonTitle string
	Section     string
	Pattern     string
	Formation   string
	Body        []byte
	Examples    []string
}

// loadGrammar 从库中取语法知识点全文与例句，作为问答的唯一上下文来源。
func (h *Handler) loadGrammar(ctx context.Context, pointID string) (*grammarCtx, error) {
	var g grammarCtx
	err := h.pool.QueryRow(ctx, `
		SELECT gp.book, gl.title, gp.section, gp.pattern, gp.formation, gp.body
		FROM grammar_point gp
		JOIN grammar_lesson gl ON gl.book = gp.book AND gl.lesson_no = gp.lesson_no
		WHERE gp.id = $1`, pointID).
		Scan(&g.Book, &g.LessonTitle, &g.Section, &g.Pattern, &g.Formation, &g.Body)
	if err != nil {
		return nil, err
	}
	rows, err := h.pool.Query(ctx, `
		SELECT speaker, mark, text, translation, note
		FROM grammar_example WHERE point_id = $1 ORDER BY seq`, pointID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var spk, mark, text, trans, note string
		if err := rows.Scan(&spk, &mark, &text, &trans, &note); err != nil {
			return nil, err
		}
		line := ""
		if spk != "" {
			line = spk + "："
		}
		if mark != "" {
			line += mark + " "
		}
		line += text
		if trans != "" {
			line += "（" + trans + "）"
		}
		if note != "" {
			line += " " + note
		}
		g.Examples = append(g.Examples, line)
		if len(g.Examples) == 10 {
			break
		}
	}
	return &g, rows.Err()
}

func (g *grammarCtx) system() string {
	sec := "语法解释"
	if g.Section == "expression" {
		sec = "表达扩展"
	}
	var b strings.Builder
	fmt.Fprintf(&b, "你是面向中文学习者的日语学习助手。学习者正在学习教材《%s》的%s，%s知识点「%s」。\n", g.Book, g.LessonTitle, sec, g.Pattern)
	if g.Formation != "" {
		fmt.Fprintf(&b, "接续：%s\n", g.Formation)
	}
	if t := bodyText(g.Body); t != "" {
		fmt.Fprintf(&b, "讲解：\n%s\n", t)
	}
	if len(g.Examples) > 0 {
		fmt.Fprintf(&b, "例句：\n%s\n", strings.Join(g.Examples, "\n"))
	}
	b.WriteString("回答要求：\n1. 用中文回答；引用日语时必须带假名注音；\n2. 围绕该知识点讲清接续、意义、使用场景与易错点，可补充 1—2 个同类例句（同样带注音与中文）；\n3. 只回答与该知识点或日语学习相关的问题，无关话题礼貌拒绝；\n4. 直接回答，不要复述本提示词。")
	return b.String()
}

type bodyBlock struct {
	Type  string              `json:"type"`
	Text  string              `json:"text"`
	Term  string              `json:"term"`
	Title string              `json:"title"`
	Head  []string            `json:"head"`
	Rows  [][]json.RawMessage `json:"rows"`
}

// bodyText 把知识点的 JSONB 讲解块压成纯文本喂给模型（ex 块由例句查询覆盖）。
func bodyText(body []byte) string {
	var blocks []bodyBlock
	if json.Unmarshal(body, &blocks) != nil {
		return ""
	}
	var b strings.Builder
	for _, blk := range blocks {
		switch blk.Type {
		case "p":
			if blk.Text != "" {
				b.WriteString(blk.Text + "\n")
			}
		case "item":
			b.WriteString(blk.Term + "：" + blk.Text + "\n")
		case "table":
			if blk.Title != "" {
				b.WriteString(blk.Title + "\n")
			}
			if len(blk.Head) > 0 {
				b.WriteString(strings.Join(blk.Head, " | ") + "\n")
			}
			for _, row := range blk.Rows {
				cells := make([]string, 0, len(row))
				for _, c := range row {
					cells = append(cells, cellText(c))
				}
				b.WriteString(strings.Join(cells, " | ") + "\n")
			}
		}
	}
	return strings.TrimSpace(b.String())
}

func cellText(raw json.RawMessage) string {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var obj struct {
		T string `json:"t"`
	}
	if json.Unmarshal(raw, &obj) == nil {
		return obj.T
	}
	return ""
}

// Ask POST /api/v1/ai/ask {kind?, text, ruby_tokens, translation, word, point_id, history, question}
// kind=grammar 时按 point_id 从库中取知识点讲解与例句，历史键为 g:<point_id>。
func (h *Handler) Ask(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Kind        string          `json:"kind"`
		Text        string          `json:"text"`
		Ruby        json.RawMessage `json:"ruby_tokens"`
		Translation string          `json:"translation"`
		Word        struct {
			Surface string `json:"surface"`
			Reading string `json:"reading"`
			Meaning string `json:"meaning"`
		} `json:"word"`
		PointID  string `json:"point_id"`
		History  []msg  `json:"history"` // 兼容旧客户端：服务端以 DB 历史为准
		Question string `json:"question"`
	}
	if e := httpx.Decode(r, &req); e != nil {
		httpx.Err(w, e)
		return
	}
	req.Kind = strings.TrimSpace(req.Kind)
	req.PointID = strings.TrimSpace(req.PointID)
	req.Question = strings.TrimSpace(req.Question)
	if req.Question == "" {
		httpx.Err(w, httpx.BadRequest("EMPTY", "问题不能为空"))
		return
	}
	if len([]rune(req.Question)) > 500 {
		httpx.Err(w, httpx.BadRequest("TOO_LONG", "问题过长（>500 字）"))
		return
	}
	var system, histKey string
	if req.Kind == "grammar" {
		if req.PointID == "" {
			httpx.Err(w, httpx.BadRequest("EMPTY", "语法点不能为空"))
			return
		}
		g, err := h.loadGrammar(r.Context(), req.PointID)
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				httpx.Err(w, httpx.NotFound("找不到这个语法点"))
			} else {
				httpx.Err(w, httpx.Internal(err.Error()))
			}
			return
		}
		system, histKey = g.system(), "g:"+req.PointID
	} else {
		if req.Text == "" {
			httpx.Err(w, httpx.BadRequest("EMPTY", "句子不能为空"))
			return
		}
		ruby := strings.TrimSpace(string(req.Ruby))
		if ruby == "" || ruby == "null" {
			ruby = "（無）"
		}
		system = sentenceSystem(req.Text, ruby, req.Translation, req.Word.Surface, req.Word.Reading, req.Word.Meaning)
		histKey = req.Text
	}

	userID := auth.FromContext(r.Context())
	if !askLim.Allow(userID) || !askDaily.Allow(userID) {
		httpx.Err(w, httpx.NewError(http.StatusTooManyRequests, "QUOTA", "提问过于频繁，请稍后再试（每日上限 100 问）"))
		return
	}
	msgs := []map[string]any{{"role": "system", "content": system}}
	// 历史以 DB 为准（跨设备留痕）：最近 20 条作上下文
	if rows, err := h.pool.Query(r.Context(), `
		SELECT role, content FROM (
			SELECT role, content, created_at FROM ai_chat
			WHERE user_id = $1 AND sentence_text = $2
			ORDER BY created_at DESC LIMIT 20
		) t ORDER BY created_at`, userID, histKey); err == nil {
		for rows.Next() {
			var role, content string
			if rows.Scan(&role, &content) == nil && content != "" {
				msgs = append(msgs, map[string]any{"role": role, "content": content})
			}
		}
		rows.Close()
	}
	msgs = append(msgs, map[string]any{"role": "user", "content": req.Question})

	body := map[string]any{
		"model":            os.Getenv("LLM_MODEL"),
		"messages":         msgs,
		"stream":           true,
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
	if resp.StatusCode != http.StatusOK {
		httpx.Err(w, httpx.BadGateway("AI 上游异常"))
		return
	}

	// SSE 代理：think=思考增量（前端实时滚动），text=正文增量，done=收尾
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Accel-Buffering", "no")
	fl, _ := w.(http.Flusher)
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 1024*1024), 1024*1024)
	var reply, reasoning strings.Builder
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if payload == "[DONE]" {
			break
		}
		var chunk struct {
			Choices []struct {
				Delta struct {
					Content   *string `json:"content"`
					Reasoning *string `json:"reasoning_content"`
				} `json:"delta"`
			} `json:"choices"`
		}
		if json.Unmarshal([]byte(payload), &chunk) != nil || len(chunk.Choices) == 0 {
			continue
		}
		dl := chunk.Choices[0].Delta
		if dl.Reasoning != nil && *dl.Reasoning != "" {
			reasoning.WriteString(*dl.Reasoning)
			eb, _ := json.Marshal(*dl.Reasoning)
			fmt.Fprintf(w, "event: think\ndata: %s\n\n", eb)
			fl.Flush()
		}
		if dl.Content != nil && *dl.Content != "" {
			reply.WriteString(*dl.Content)
			cb, _ := json.Marshal(*dl.Content)
			fmt.Fprintf(w, "event: text\ndata: %s\n\n", cb)
			fl.Flush()
		}
	}
	// 上游断流时 scanner 会带错误退出：不能把半截回答当完整回复入库。
	if err := sc.Err(); err != nil {
		fmt.Fprintf(w, "event: error\ndata: %s\n\n", mustJSON(map[string]any{"message": "AI 输出中断，请重试"}))
		fl.Flush()
		return
	}
	if reply.Len() == 0 {
		fmt.Fprintf(w, "event: error\ndata: %s\n\n", mustJSON(map[string]any{"message": "AI 返回空回复"}))
		fl.Flush()
		return
	}
	// 对话留痕：问与答各落一行（跨设备可回看）
	_, _ = h.pool.Exec(r.Context(), `
		INSERT INTO ai_chat (id, user_id, sentence_text, role, content)
		VALUES ($1, $2, $3, 'user', $4)`,
		ulid.MustNew(), userID, histKey, req.Question)
	_, _ = h.pool.Exec(r.Context(), `
		INSERT INTO ai_chat (id, user_id, sentence_text, role, content)
		VALUES ($1, $2, $3, 'assistant', $4)`,
		ulid.MustNew(), userID, histKey, reply.String())
	_, _ = h.pool.Exec(r.Context(), `
		INSERT INTO llm_call_log (id, user_id, lesson_id, model, prompt_tokens, output_tokens, duration_ms)
		VALUES ($1, $2, NULL, $3, $4, $5, $6)`,
		ulid.MustNew(), userID, os.Getenv("LLM_MODEL"),
		len([]rune(system+req.Question)), len([]rune(reply.String())), time.Since(start).Milliseconds())
	fmt.Fprintf(w, "event: done\ndata: %s\n\n", mustJSON(map[string]any{"reply": reply.String(), "reasoning": reasoning.String()}))
	fl.Flush()
}

func mustJSON(v any) []byte {
	b, _ := json.Marshal(v)
	return b
}

// Chat GET /api/v1/ai/chat?text= ：某句的聊天记录（升序，最近 100 条）
func (h *Handler) Chat(w http.ResponseWriter, r *http.Request) {
	userID := auth.FromContext(r.Context())
	text := r.URL.Query().Get("text")
	rows, err := h.pool.Query(r.Context(), `
		SELECT role, content, created_at FROM ai_chat
		WHERE user_id = $1 AND sentence_text = $2
		ORDER BY created_at LIMIT 100`, userID, text)
	if err != nil {
		httpx.Err(w, httpx.Internal(err.Error()))
		return
	}
	defer rows.Close()
	items := []map[string]any{}
	for rows.Next() {
		var role, content string
		var at time.Time
		if err := rows.Scan(&role, &content, &at); err != nil {
			httpx.Err(w, httpx.Internal(err.Error()))
			return
		}
		items = append(items, map[string]any{"role": role, "content": content, "at": at.Format(time.RFC3339)})
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"items": items})
}

// ChatClear DELETE /api/v1/ai/chat?text= ：清空某句的聊天记录
func (h *Handler) ChatClear(w http.ResponseWriter, r *http.Request) {
	userID := auth.FromContext(r.Context())
	text := r.URL.Query().Get("text")
	if _, err := h.pool.Exec(r.Context(), `DELETE FROM ai_chat WHERE user_id = $1 AND sentence_text = $2`, userID, text); err != nil {
		httpx.Err(w, httpx.Internal(err.Error()))
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"ok": true})
}
