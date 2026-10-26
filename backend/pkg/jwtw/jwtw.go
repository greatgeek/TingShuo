// Package jwtw 提供 JWT 签发与解析（§8.6：客户端只持短效 JWT + refresh token）。
package jwtw

import (
	"errors"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

// Claims 是统一声明结构；Type 区分 access / refresh。
type Claims struct {
	UserID string `json:"uid"`
	Type   string `json:"typ"` // "access" | "refresh"
	jwt.RegisteredClaims
}

const (
	TypeAccess  = "access"
	TypeRefresh = "refresh"

	AccessTTL  = 2 * time.Hour
	RefreshTTL = 30 * 24 * time.Hour
)

var ErrInvalid = errors.New("jwt: 无效或过期")

// Signer 签发与校验 JWT（HS256 + JWT_SECRET）。
type Signer struct{ secret []byte }

func New(secret string) *Signer { return &Signer{secret: []byte(secret)} }

// Issue 签发一个指定类型的 token（jti 用 ULID，由调用方传入）。
func (s *Signer) Issue(userID, typ, jti string, ttl time.Duration) (string, error) {
	now := time.Now()
	claims := Claims{
		UserID: userID,
		Type:   typ,
		RegisteredClaims: jwt.RegisteredClaims{
			ID:        jti,
			Subject:   userID,
			IssuedAt:  jwt.NewNumericDate(now),
			ExpiresAt: jwt.NewNumericDate(now.Add(ttl)),
		},
	}
	return jwt.NewWithClaims(jwt.SigningMethodHS256, claims).SignedString(s.secret)
}

// Parse 解析并校验 token，返回声明。
func (s *Signer) Parse(tokenStr string) (*Claims, error) {
	token, err := jwt.ParseWithClaims(tokenStr, &Claims{}, func(t *jwt.Token) (any, error) {
		if _, ok := t.Method.(*jwt.SigningMethodHMAC); !ok {
			return nil, ErrInvalid
		}
		return s.secret, nil
	})
	if err != nil || !token.Valid {
		return nil, ErrInvalid
	}
	claims, ok := token.Claims.(*Claims)
	if !ok {
		return nil, ErrInvalid
	}
	return claims, nil
}

// IssuePair 签发 access + refresh 双 token。
func (s *Signer) IssuePair(userID string, newJTI func() string) (access, refresh string, err error) {
	if access, err = s.Issue(userID, TypeAccess, newJTI(), AccessTTL); err != nil {
		return
	}
	refresh, err = s.Issue(userID, TypeRefresh, newJTI(), RefreshTTL)
	return
}
