// Package llmclient 提供 OpenAI 兼容 LLM 客户端（BYOK：Key 由调用方从本地 .env 提供，
// 生产环境由客户端持有；服务端代码不持有任何 LLM Key）。
package llmclient

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

type Client struct {
	baseURL string // 形如 https://api.minimaxi.com/v1（不带尾斜杠）
	apiKey  string
	model   string
	http    *http.Client
}

func New(baseURL, apiKey, model string) *Client {
	return &Client{
		baseURL: strings.TrimSuffix(baseURL, "/"),
		apiKey:  apiKey,
		model:   model,
		// 推理模型（如 MiniMax M3）非流式响应需等全部 reasoning+content token 生成完，
		// 大材料包（~4K out + reasoning）可达数分钟
		http: &http.Client{Timeout: 600 * time.Second},
	}
}

type chatResponse struct {
	Choices []struct {
		Message struct {
			Content string `json:"content"`
		} `json:"message"`
	} `json:"choices"`
	Usage struct {
		PromptTokens     int `json:"prompt_tokens"`
		CompletionTokens int `json:"completion_tokens"`
		TotalTokens      int `json:"total_tokens"`
	} `json:"usage"`
}

// ChatJSON 发送单轮 user prompt 并返回模型回复文本。
// 返回的 prompt/completion token 数供成本观测（llm_call_log）。
func (c *Client) ChatJSON(ctx context.Context, prompt string) (content string, promptTokens, completionTokens int, err error) {
	body, err := json.Marshal(map[string]any{
		"model": c.model,
		"messages": []map[string]string{
			{"role": "user", "content": prompt},
		},
	})
	if err != nil {
		return "", 0, 0, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		c.baseURL+"/chat/completions", bytes.NewReader(body))
	if err != nil {
		return "", 0, 0, err
	}
	req.Header.Set("Authorization", "Bearer "+c.apiKey)
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.http.Do(req)
	if err != nil {
		return "", 0, 0, fmt.Errorf("LLM 请求失败: %w", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return "", 0, 0, fmt.Errorf("LLM 返回 %d: %s", resp.StatusCode, truncate(string(raw), 300))
	}
	var cr chatResponse
	if err := json.Unmarshal(raw, &cr); err != nil {
		return "", 0, 0, fmt.Errorf("LLM 响应解析失败: %w", err)
	}
	if len(cr.Choices) == 0 {
		return "", 0, 0, fmt.Errorf("LLM 响应缺少 choices: %s", truncate(string(raw), 300))
	}
	return cr.Choices[0].Message.Content, cr.Usage.PromptTokens, cr.Usage.CompletionTokens, nil
}

// ExtractJSON 从模型回复中提取 JSON 主体。
// 推理模型（如 MiniMax M3）可能把思考文本混进 content 并与 JSON 交错，
// 因此采用「最大平衡块」策略：扫描全部平衡的 {...} 子串，取其中合法且最长的。
func ExtractJSON(s string) string {
	// 剥离 markdown 围栏（若有）
	if i := strings.Index(s, "```json"); i >= 0 {
		s = s[i+len("```json"):]
		if j := strings.Index(s, "```"); j >= 0 {
			s = s[:j]
		}
	} else if i := strings.Index(s, "```"); i >= 0 {
		s = s[i+3:]
		if j := strings.Index(s, "```"); j >= 0 {
			s = s[:j]
		}
	}
	if best := largestBalancedJSON(s); best != "" {
		return best
	}
	// 兜底：首个 { 到末个 }
	start := strings.Index(s, "{")
	end := strings.LastIndex(s, "}")
	if start >= 0 && end > start {
		return s[start : end+1]
	}
	return s
}

// largestBalancedJSON 返回 s 中合法且最长的平衡 {...} 子串（O(n²)，输入通常 < 32KB）。
func largestBalancedJSON(s string) string {
	best := ""
	for i := 0; i < len(s); i++ {
		if s[i] != '{' {
			continue
		}
		depth, inStr, esc := 0, false, false
	matched:
		for j := i; j < len(s); j++ {
			c := s[j]
			if esc {
				esc = false
				continue
			}
			if inStr {
				if c == '\\' {
					esc = true
				} else if c == '"' {
					inStr = false
				}
				continue
			}
			switch c {
			case '"':
				inStr = true
			case '{':
				depth++
			case '}':
				depth--
				if depth == 0 {
					if cand := s[i : j+1]; len(cand) > len(best) && json.Valid([]byte(cand)) {
						best = cand
					}
					break matched
				}
			}
		}
	}
	return best
}

func truncate(s string, n int) string {
	if len(s) > n {
		return s[:n]
	}
	return s
}
