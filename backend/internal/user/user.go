// Package user 实现 GET/PUT /me（FR-10 偏好）。
package user

import (
	"encoding/json"
	"net/http"

	"github.com/greatgeek/TingShuo/backend/internal/auth"
	"github.com/greatgeek/TingShuo/backend/internal/httpx"
	"github.com/jackc/pgx/v5/pgxpool"
)

type Handler struct{ pool *pgxpool.Pool }

func NewHandler(pool *pgxpool.Pool) *Handler { return &Handler{pool: pool} }

type profile struct {
	ID             string          `json:"id"`
	Email          string          `json:"email"`
	Nickname       *string         `json:"nickname"`
	TargetLanguage string          `json:"target_language"`
	Level          *string         `json:"level"`
	ScenePrefs     json.RawMessage `json:"scene_prefs"`
	StylePrefs     json.RawMessage `json:"style_prefs"`
	TTSAutoGen     bool            `json:"tts_auto_gen"`
}

// Me GET /me（scene/style 以原始 JSON 下发）
func (h *Handler) Me(w http.ResponseWriter, r *http.Request) {
	var p profile
	err := h.pool.QueryRow(r.Context(), `
		SELECT id, email, nickname, target_language, level, scene_prefs, style_prefs, tts_auto_gen
		FROM "user" WHERE id = $1 AND deleted_at IS NULL`,
		auth.FromContext(r.Context())).Scan(
		&p.ID, &p.Email, &p.Nickname, &p.TargetLanguage, &p.Level, &p.ScenePrefs, &p.StylePrefs, &p.TTSAutoGen)
	if err != nil {
		httpx.Err(w, httpx.NotFound("用户不存在"))
		return
	}
	httpx.WriteJSON(w, http.StatusOK, p)
}

// Update PUT /me {nickname?, level?, scene_prefs?, style_prefs?, tts_auto_gen?}
// 未传的字段保持原值（COALESCE 逐字段更新）。
func (h *Handler) Update(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Nickname   *string         `json:"nickname"`
		Level      *string         `json:"level"`
		ScenePrefs []string        `json:"scene_prefs"`
		StylePrefs json.RawMessage `json:"style_prefs"`
		TTSAutoGen *bool           `json:"tts_auto_gen"`
	}
	if e := httpx.Decode(r, &req); e != nil {
		httpx.Err(w, e)
		return
	}
	if req.Level != nil {
		switch *req.Level {
		case "N5", "N4", "N3":
		default:
			httpx.Err(w, httpx.BadRequest("LEVEL_INVALID", "level 仅支持 N5/N4/N3"))
			return
		}
	}
	_, err := h.pool.Exec(r.Context(), `
		UPDATE "user" SET
			nickname     = COALESCE($2, nickname),
			level        = COALESCE($3, level),
			scene_prefs  = COALESCE($4::jsonb, scene_prefs),
			style_prefs  = COALESCE($5::jsonb, style_prefs),
			tts_auto_gen = COALESCE($6, tts_auto_gen),
			updated_at   = now()
		WHERE id = $1`,
		auth.FromContext(r.Context()), req.Nickname, req.Level,
		toJSONB(req.ScenePrefs), []byte(req.StylePrefs), req.TTSAutoGen)
	if err != nil {
		httpx.Err(w, httpx.Internal("更新失败: "+err.Error()))
		return
	}
	h.Me(w, r)
}

// toJSONB：nil 切片 → nil（SQL NULL，不更新）；否则 marshal 为 JSON。
func toJSONB(v []string) []byte {
	if v == nil {
		return nil
	}
	b, err := json.Marshal(v)
	if err != nil {
		return nil
	}
	return b
}
