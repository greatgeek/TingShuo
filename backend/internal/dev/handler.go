// Package dev 提供仅限本地开发的浏览器试用代理。
//
// 为什么存在：生产架构是 BYOK——Web 客户端在浏览器里直连 LLM/Azure（FR-3.12）。
// 浏览器直连的 CORS 可行性属硬风险（部署文档 §6.4），开发期用本代理绕过，
// Key 仍只来自 .env.dev（冒烟调试用途，部署文档 §2.3），服务端不落库、不记日志。
//
// 生产部署必须设置 DEV_PROXY=false 关闭本包全部端点。
package dev

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"

	"github.com/greatgeek/TingShuo/backend/internal/httpx"
	"github.com/greatgeek/TingShuo/backend/pkg/ttsclient"
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
	azure *ttsclient.Client
}

func NewHandler() *Handler {
	return &Handler{azure: ttsclient.New(
		os.Getenv("AZURE_SPEECH_KEY"), os.Getenv("AZURE_SPEECH_REGION"))}
}

// Config 返回试用页默认配置（含 .env.dev 的 Key，仅供本地页面预填）。
func (h *Handler) Config(w http.ResponseWriter, r *http.Request) {
	httpx.WriteJSON(w, http.StatusOK, map[string]string{
		"llm_base_url":   os.Getenv("LLM_BASE_URL"),
		"llm_model":      os.Getenv("LLM_MODEL"),
		"llm_api_key":    os.Getenv("LLM_API_KEY"),
		"azure_region":   os.Getenv("AZURE_SPEECH_REGION"),
		"azure_key":      os.Getenv("AZURE_SPEECH_KEY"),
	})
}

// LLMProxy 转发 chat/completions（body 原样透传，Key 从 env）。
func (h *Handler) LLMProxy(w http.ResponseWriter, r *http.Request) {
	target := strings.TrimSuffix(os.Getenv("LLM_BASE_URL"), "/") + "/chat/completions"
	body, err := io.ReadAll(io.LimitReader(r.Body, 4<<20))
	if err != nil {
		httpx.Err(w, httpx.BadRequest("READ_ERR", err.Error()))
		return
	}
	req, err := http.NewRequestWithContext(r.Context(), http.MethodPost, target, bytes.NewReader(body))
	if err != nil {
		httpx.Err(w, httpx.Internal(err.Error()))
		return
	}
	req.Header.Set("Authorization", "Bearer "+os.Getenv("LLM_API_KEY"))
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req) // 推理模型可达数分钟，用长超时客户端
	if err != nil {
		httpx.Err(w, httpx.BadGateway("LLM 上游请求失败: " + err.Error()))
		return
	}
	defer resp.Body.Close()
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(resp.StatusCode)
	io.Copy(w, resp.Body)
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
