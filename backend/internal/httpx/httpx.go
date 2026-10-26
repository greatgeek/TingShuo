// Package httpx 提供统一 JSON 响应与错误体（§6.3：{"error":{"code","message"}}）。
package httpx

import (
	"encoding/json"
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

// WriteJSON 输出 JSON 响应。
func WriteJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// Err 输出统一错误体。
func Err(w http.ResponseWriter, e *APIError) {
	WriteJSON(w, e.Status, map[string]any{
		"error": map[string]string{"code": e.Code, "message": e.Message},
	})
}

// Decode 严格解析请求体（禁止未知字段，防参数走私）。
func Decode(r *http.Request, v any) *APIError {
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return BadRequest("BAD_REQUEST", "请求体解析失败: "+err.Error())
	}
	return nil
}
