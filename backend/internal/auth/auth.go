// Package auth 实现注册登录与 JWT 鉴权中间件（FR-9.1）。
package auth

import (
	"context"
	"net/http"
	"strings"

	"github.com/greatgeek/TingShuo/backend/internal/httpx"
	"github.com/greatgeek/TingShuo/backend/pkg/jwtw"
	"github.com/greatgeek/TingShuo/backend/pkg/passhash"
	"github.com/greatgeek/TingShuo/backend/pkg/ulid"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Handler 处理 auth 相关请求。
type Handler struct {
	pool   *pgxpool.Pool
	signer *jwtw.Signer
}

func NewHandler(pool *pgxpool.Pool, signer *jwtw.Signer) *Handler {
	return &Handler{pool: pool, signer: signer}
}

type credentials struct {
	Email    string `json:"email"`
	Password string `json:"password"`
}

type tokenResp struct {
	UserID   string `json:"user_id"`
	Access   string `json:"access_token"`
	Refresh  string `json:"refresh_token"`
	ExpireIn  int64  `json:"expire_in"` // access 有效秒数
}

// Register POST /auth/register {email, password, nickname?}
func (h *Handler) Register(w http.ResponseWriter, r *http.Request) {
	var req struct {
		credentials
		Nickname string `json:"nickname"`
	}
	if e := httpx.Decode(r, &req); e != nil {
		httpx.Err(w, e)
		return
	}
	req.Email = strings.TrimSpace(strings.ToLower(req.Email))
	if !strings.Contains(req.Email, "@") || len(req.Email) < 5 {
		httpx.Err(w, httpx.BadRequest("EMAIL_INVALID", "邮箱格式不合法"))
		return
	}
	if len(req.Password) < 8 {
		httpx.Err(w, httpx.BadRequest("PASSWORD_TOO_SHORT", "密码至少 8 位"))
		return
	}

	hash, err := passhash.Hash(req.Password)
	if err != nil {
		httpx.Err(w, httpx.Internal("密码哈希失败"))
		return
	}
	id := ulid.MustNew()
	_, err = h.pool.Exec(r.Context(),
		`INSERT INTO "user" (id, email, password_hash, nickname) VALUES ($1, $2, $3, $4)`,
		id, req.Email, hash, req.Nickname)
	if err != nil {
		if strings.Contains(err.Error(), "duplicate key") {
			httpx.Err(w, httpx.Conflict("EMAIL_TAKEN", "该邮箱已注册"))
			return
		}
		httpx.Err(w, httpx.Internal("注册失败: "+err.Error()))
		return
	}
	h.writeTokens(w, http.StatusCreated, id)
}

// Login POST /auth/login {email, password}
func (h *Handler) Login(w http.ResponseWriter, r *http.Request) {
	var req credentials
	if e := httpx.Decode(r, &req); e != nil {
		httpx.Err(w, e)
		return
	}
	req.Email = strings.TrimSpace(strings.ToLower(req.Email))

	var id, hash string
	err := h.pool.QueryRow(r.Context(),
		`SELECT id, password_hash FROM "user" WHERE email = $1 AND deleted_at IS NULL`,
		req.Email).Scan(&id, &hash)
	if err != nil || !passhash.Verify(hash, req.Password) {
		httpx.Err(w, httpx.Unauthorized("邮箱或密码不正确"))
		return
	}
	h.writeTokens(w, http.StatusOK, id)
}

func (h *Handler) writeTokens(w http.ResponseWriter, status int, userID string) {
	access, refresh, err := h.signer.IssuePair(userID, ulid.MustNew)
	if err != nil {
		httpx.Err(w, httpx.Internal("签发 token 失败"))
		return
	}
	httpx.WriteJSON(w, status, tokenResp{
		UserID: userID, Access: access, Refresh: refresh,
		ExpireIn: int64(jwtw.AccessTTL.Seconds()),
	})
}

// Refresh POST /auth/refresh {refresh_token} → 换发新 access
func (h *Handler) Refresh(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Refresh string `json:"refresh_token"`
	}
	if e := httpx.Decode(r, &req); e != nil {
		httpx.Err(w, e)
		return
	}
	claims, err := h.signer.Parse(req.Refresh)
	if err != nil || claims.Type != jwtw.TypeRefresh {
		httpx.Err(w, httpx.Unauthorized("refresh token 无效或已过期"))
		return
	}
	access, _, err := h.signer.IssuePair(claims.UserID, ulid.MustNew)
	if err != nil {
		httpx.Err(w, httpx.Internal("签发 token 失败"))
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"access_token": access})
}

// ---- 鉴权中间件 ----

type ctxKey struct{}

// Middleware 校验 Authorization: Bearer <access>，注入 user_id。
func (h *Handler) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		authz := r.Header.Get("Authorization")
		token, ok := strings.CutPrefix(authz, "Bearer ")
		if !ok {
			httpx.Err(w, httpx.Unauthorized("缺少 Bearer token"))
			return
		}
		claims, err := h.signer.Parse(token)
		if err != nil {
			httpx.Err(w, httpx.Unauthorized("token 无效或已过期"))
			return
		}
		if claims.Type != jwtw.TypeAccess {
			httpx.Err(w, httpx.Unauthorized("需要 access token"))
			return
		}
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), ctxKey{}, claims.UserID)))
	})
}

// FromContext 取出中间件注入的 user_id。
func FromContext(ctx context.Context) string {
	id, _ := ctx.Value(ctxKey{}).(string)
	return id
}
