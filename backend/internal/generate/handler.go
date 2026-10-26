// 材料上传：POST /lessons/{id}/materials（BYOK §3：客户端生成 → 服务端校验+增强+落库）。
package generate

import (
	"encoding/json"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/greatgeek/TingShuo/backend/internal/auth"
	"github.com/greatgeek/TingShuo/backend/internal/httpx"
	"github.com/greatgeek/TingShuo/backend/pkg/ulid"
	"github.com/jackc/pgx/v5/pgxpool"
)

type Handler struct{ pool *pgxpool.Pool }

func NewHandler(pool *pgxpool.Pool) *Handler { return &Handler{pool: pool} }

// Upload 校验、增强（振り仮名）、落库材料包，返回 临时ID→真实ID 映射与校验 warnings。
func (h *Handler) Upload(w http.ResponseWriter, r *http.Request) {
	userID := auth.FromContext(r.Context())
	lessonID := chi.URLParam(r, "id")

	// 课程归属 + 状态检查（重复上传拒绝，单材料重生成走 P1 重建课）
	var status string
	err := h.pool.QueryRow(r.Context(), `
		SELECT status FROM daily_lesson WHERE id = $1 AND user_id = $2`,
		lessonID, userID).Scan(&status)
	if err != nil {
		httpx.Err(w, httpx.NotFound("课程不存在或不属于当前用户"))
		return
	}
	if status == "ready" {
		httpx.Err(w, httpx.Conflict("MATERIALS_EXISTS", "材料已上传（重新生成需删除课程重建）"))
		return
	}

	// 本课词集合（wordID → isKey）
	words := map[string]bool{}
	wrows, err := h.pool.Query(r.Context(),
		`SELECT word_id, is_key FROM lesson_word WHERE lesson_id = $1`, lessonID)
	if err != nil {
		httpx.Err(w, httpx.Internal(err.Error()))
		return
	}
	for wrows.Next() {
		var wid string
		var isKey bool
		if err := wrows.Scan(&wid, &isKey); err != nil {
			httpx.Err(w, httpx.Internal(err.Error()))
			return
		}
		words[wid] = isKey
	}
	wrows.Close()

	var out GenerationOutput
	if e := httpx.Decode(r, &out); e != nil {
		httpx.Err(w, e)
		return
	}
	if problems := out.Validate(words); len(problems) > 0 {
		httpx.WriteJSON(w, http.StatusBadRequest, map[string]any{
			"error": map[string]string{
				"code": "MATERIALS_INVALID", "message": "材料校验未通过（schema 见 §5.3）"},
			"problems": problems,
		})
		return
	}

	// ---- 落库（事务）----
	tx, err := h.pool.Begin(r.Context())
	if err != nil {
		httpx.Err(w, httpx.Internal(err.Error()))
		return
	}
	defer tx.Rollback(r.Context())
	ctx := r.Context()

	matIDMap := map[string]string{}  // mid → 真实 material id
	sentIDMap := map[string]string{} // sid → 真实 sentence id
	var warnings []string

	for _, m := range out.Materials {
		realMatID := ulid.MustNew()
		matIDMap[m.ID] = realMatID
		if _, err = tx.Exec(ctx, `
			INSERT INTO material (id, lesson_id, type, scene, role_a, role_b, title, sort_order)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8)`,
			realMatID, lessonID, m.Type, m.Scene, m.RoleA, m.RoleB, m.Title, m.SortOrder); err != nil {
			httpx.Err(w, httpx.Internal("材料落库失败: "+err.Error()))
			return
		}
		for _, s := range m.Sentences {
			ruby := BuildRubyTokens(s.Text)
			if ratio := KanaCrossCheck(ruby, s.Kana); ratio < 0.5 && s.Kana != "" {
				warnings = append(warnings, "kana_mismatch:"+s.ID)
			}
			rubyJSON, _ := json.Marshal(ruby)
			realSentID := ulid.MustNew()
			sentIDMap[s.ID] = realSentID
			if _, err = tx.Exec(ctx, `
				INSERT INTO sentence (id, material_id, lesson_id, sort_order, speaker, text, kana, ruby_tokens, translation, para_no)
				VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)`,
				realSentID, realMatID, lessonID, s.SortOrder, s.Speaker, s.Text, s.Kana, rubyJSON, s.Translation, s.ParaNo); err != nil {
				httpx.Err(w, httpx.Internal("句子落库失败: "+err.Error()))
				return
			}
		}
	}

	for i, qa := range out.QAItems {
		if _, err = tx.Exec(ctx, `
			INSERT INTO qa_item (id, lesson_id, type, prompt, prompt_translation, answer, sort_order)
			VALUES ($1, $2, 'qa', $3, $4, $5, $6)`,
			ulid.MustNew(), lessonID, qa.Prompt, qa.PromptZh, qa.Answer, i+1); err != nil {
			httpx.Err(w, httpx.Internal("问答落库失败: "+err.Error()))
			return
		}
	}
	for i, sub := range out.Substitutions {
		if _, err = tx.Exec(ctx, `
			INSERT INTO qa_item (id, lesson_id, type, prompt, prompt_translation, answer, sort_order)
			VALUES ($1, $2, 'substitution', $3, $4, $5, $6)`,
			ulid.MustNew(), lessonID, sub.WordID, sub.PromptZh, sub.Answer, i+1); err != nil {
			httpx.Err(w, httpx.Internal("造句任务落库失败: "+err.Error()))
			return
		}
	}

	// 更新 lesson_word 的复现统计与 skipped 状态
	for wid, occ := range out.WordOccurrence {
		skipReason := occ.SkipReason
		if _, err = tx.Exec(ctx, `
			UPDATE lesson_word SET
				occurrence_count = $3, skipped = $4, skip_reason = $5
			WHERE lesson_id = $1 AND word_id = $2`,
			lessonID, wid, len(occ.Sentences), occ.Skipped, skipReason); err != nil {
			httpx.Err(w, httpx.Internal("词复现统计更新失败: "+err.Error()))
			return
		}
	}

	if _, err = tx.Exec(ctx, `
		UPDATE daily_lesson SET status = 'ready', updated_at = now() WHERE id = $1`,
		lessonID); err != nil {
		httpx.Err(w, httpx.Internal(err.Error()))
		return
	}
	if err = tx.Commit(ctx); err != nil {
		httpx.Err(w, httpx.Internal(err.Error()))
		return
	}

	if warnings == nil {
		warnings = []string{}
	}
	httpx.WriteJSON(w, http.StatusCreated, map[string]any{
		"status":        "ready",
		"material_ids":  matIDMap,
		"sentence_ids":  sentIDMap,
		"warnings":       warnings,
	})
}
