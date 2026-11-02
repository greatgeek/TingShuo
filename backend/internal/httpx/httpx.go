// Package httpx 提供统一 JSON 响应与错误体（§6.3：{"error":{"code","message"}}）。
package httpx

import (
	"encoding/json"
	"io"
	"log"
	"net/http"
)

// APIError 是业务错误（code 为稳定错误码，供客户端分支处理）。
type APIError struct {
	Status  int
	Code    string
	Message string
}

func (e *APIError) Error() string { return e.Code + ": " + e.Message }

func NewError(status int, code, message string) *APIError {
	return &APIError{Status: status, Code: code, Message: message}
}

// 常用错误构造器
func BadRequest(code, msg string) *APIError { return NewError(400, code, msg) }
func Unauthorized(msg string) *APIError     { return NewError(401, "UNAUTHORIZED", msg) }
func Forbidden(msg string) *APIError        { return NewError(403, "FORBIDDEN", msg) }
func NotFound(msg string) *APIError         { return NewError(404, "NOT_FOUND", msg) }
func Conflict(code, msg string) *APIError   { return NewError(409, code, msg) }
func Internal(msg string) *APIError         { return NewError(500, "INTERNAL", msg) }
func BadGateway(msg string) *APIError       { return NewError(502, "BAD_GATEWAY", msg) }

// WriteJSON 输出 JSON 响应。
func WriteJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// Err 输出统一错误体。5xx 一律脱敏：细节只进服务端日志，防信息泄漏。
func Err(w http.ResponseWriter, e *APIError) {
	if e.Status >= 500 {
		log.Printf("httpx %d %s: %s", e.Status, e.Code, e.Message)
		e = NewError(e.Status, e.Code, "服务器内部错误，请稍后重试")
	}
	WriteJSON(w, e.Status, map[string]any{
		"error": map[string]string{"code": e.Code, "message": e.Message},
	})
}

const maxBodyBytes = 1 << 20 // 1MB：防超大请求体耗尽内存

func truncated(err error) string {
	m := err.Error()
	if len([]rune(m)) > 120 {
		m = string([]rune(m)[:120]) + "…"
	}
	return m
}

// Decode 严格解析请求体（禁止未知字段，防参数走私）。
func Decode(r *http.Request, v any) *APIError {
	dec := json.NewDecoder(io.LimitReader(r.Body, maxBodyBytes))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return BadRequest("BAD_REQUEST", "请求体解析失败: "+truncated(err))
	}
	return nil
}

// DecodeLoose 宽松解析（忽略未知字段）——用于接收 LLM 原始输出的端点：
// 模型会自作主张附加 schema 外字段（如 "topic"），显式校验由各 handler 自行完成。
func DecodeLoose(r *http.Request, v any) *APIError {
	if err := json.NewDecoder(io.LimitReader(r.Body, maxBodyBytes)).Decode(v); err != nil {
		return BadRequest("BAD_REQUEST", "请求体解析失败: "+truncated(err))
	}
	return nil
}
