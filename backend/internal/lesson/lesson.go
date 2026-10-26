// Package lesson 实现课程 CRUD：幂等建课（§6.3）、词单入库、全量查询（§6.2）。
package lesson

import (
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/greatgeek/TingShuo/backend/internal/auth"
	"github.com/greatgeek/TingShuo/backend/internal/httpx"
	"github.com/greatgeek/TingShuo/backend/pkg/ulid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

type Handler struct{ pool *pgxpool.Pool }

func NewHandler(pool *pgxpool.Pool) *Handler { return &Handler{pool: pool} }

// ---- POST /lessons：提交词单建课（status=draft）----

type createReq struct {
	LessonDate string            `json:"lesson_date"` // YYYY-MM-DD
	Words      []WordInput       `json:"words"`
	Params     map[string]any    `json:"params"` // 生成参数快照（scenes/level/context...）
}

// Create 幂等：同日重复提交返回既有课程 + 409（§6.3）。
func (h *Handler) Create(w http.ResponseWriter, r *http.Request) {
	userID := auth.FromContext(r.Context())
	var req createReq
	if e := httpx.Decode(r, &req); e != nil {
		httpx.Err(w, e)
		return
	}

	words, ve := ValidateWords(req.Words)
	if ve != nil {
		httpx.Err(w, ve)
		return
	}
	date, err := time.Parse("2006-01-02", req.LessonDate)
	if err != nil {
		httpx.Err(w, httpx.BadRequest("DATE_INVALID", "lesson_date 必须为 YYYY-MM-DD"))
		return
	}

	paramsJSON, _ := json.Marshal(req.Params)

	// 同日幂等：已有课程时——draft 允许重新提交词单（试用反馈 2026-10-26），
	// 非 draft（ready 等）保持一天一课语义（§6.3）
	var existingID, existingStatus string
	err = h.pool.QueryRow(r.Context(), `
		SELECT id, status FROM daily_lesson WHERE user_id = $1 AND lesson_date = $2`,
		userID, date).Scan(&existingID, &existingStatus)
	if err != nil && err != pgx.ErrNoRows {
		httpx.Err(w, httpx.Internal("查询当日课程失败: "+err.Error()))
		return
	}

	if existingID != "" {
		if existingStatus != "draft" {
			httpx.WriteJSON(w, http.StatusConflict, map[string]any{
				"lesson_id": existingID,
				"existing":  true,
				"error":     map[string]string{"code": "LESSON_EXISTS", "message": "当日课程已存在（一天一课）"},
			})
			return
		}
		if err := h.replaceDraftWords(r, existingID, userID, words, paramsJSON); err != nil {
			httpx.Err(w, httpx.Internal("更新词单失败: "+err.Error()))
			return
		}
		httpx.WriteJSON(w, http.StatusOK, map[string]any{
			"lesson_id": existingID, "status": "draft", "word_count": len(words), "updated": true,
		})
		return
	}

	lessonID := ulid.MustNew()
	if _, err := h.pool.Exec(r.Context(), `
		INSERT INTO daily_lesson (id, user_id, lesson_date, status, word_count, generate_params)
		VALUES ($1, $2, $3, 'draft', $4, $5)`,
		lessonID, userID, date, len(words), paramsJSON); err != nil {
		httpx.Err(w, httpx.Internal("建课失败: "+err.Error()))
		return
	}
	if err := h.replaceDraftWords(r, lessonID, userID, words, paramsJSON); err != nil {
		httpx.Err(w, httpx.Internal("词单入库失败: "+err.Error()))
		return
	}

	httpx.WriteJSON(w, http.StatusCreated, map[string]any{
		"lesson_id": lessonID,
		"status":     "draft",
		"word_count": len(words),
	})
}

// replaceDraftWords 词入库 + lesson_word 全量重建（新课后提交；draft 课重新提交词单时复用）。
func (h *Handler) replaceDraftWords(r *http.Request, lessonID, userID string, words []WordInput, paramsJSON []byte) error {
	tx, err := h.pool.Begin(r.Context())
	if err != nil {
		return err
	}
	defer tx.Rollback(r.Context())
	ctx := r.Context()

	// draft 课尚未生成材料（无 sentence 引用），旧词关联可安全清除
	if _, err := tx.Exec(ctx, `DELETE FROM lesson_word WHERE lesson_id = $1`, lessonID); err != nil {
		return err
	}
	for _, wi := range words {
		wordID := ulid.MustNew()
		var wid string
		// word 全局词库 upsert：同 (user_id, surface, reading) 更新词性与释义（reading 空串方案）
		err = tx.QueryRow(ctx, `
			INSERT INTO word (id, user_id, surface, reading, pos, meaning)
			VALUES ($1, $2, $3, $4, $5, $6)
			ON CONFLICT (user_id, surface, reading) DO UPDATE
			  SET pos = EXCLUDED.pos, meaning = EXCLUDED.meaning, updated_at = now()
			RETURNING id`,
			wordID, userID, wi.Surface, wi.Reading, wi.Pos, wi.Meaning).Scan(&wid)
		if err != nil {
			return fmt.Errorf("词入库失败: %w", err)
		}
		if _, err = tx.Exec(ctx, `
			INSERT INTO lesson_word (lesson_id, word_id, is_key)
			VALUES ($1, $2, $3)
			ON CONFLICT (lesson_id, word_id) DO NOTHING`,
			lessonID, wid, wi.IsKey); err != nil {
			return fmt.Errorf("词单关联失败: %w", err)
		}
	}
	if _, err := tx.Exec(ctx, `
		UPDATE daily_lesson SET word_count = $2, generate_params = $3, updated_at = now() WHERE id = $1`,
		lessonID, len(words), paramsJSON); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// ---- GET /lessons?from=&to= ----

func (h *Handler) List(w http.ResponseWriter, r *http.Request) {
	userID := auth.FromContext(r.Context())
	from, to := r.URL.Query().Get("from"), r.URL.Query().Get("to")
	if from == "" {
		from = "0001-01-01"
	}
	if to == "" {
		to = "9999-12-31"
	}

	rows, err := h.pool.Query(r.Context(), `
		SELECT id, lesson_date::text, status, word_count
		FROM daily_lesson
		WHERE user_id = $1 AND lesson_date BETWEEN $2 AND $3
		ORDER BY lesson_date DESC LIMIT 100`,
		userID, from, to)
	if err != nil {
		httpx.Err(w, httpx.Internal(err.Error()))
		return
	}
	defer rows.Close()

	items := []map[string]any{}
	for rows.Next() {
		var id, date, status string
		var wc *int
		if err := rows.Scan(&id, &date, &status, &wc); err != nil {
			httpx.Err(w, httpx.Internal(err.Error()))
			return
		}
		items = append(items, map[string]any{
			"lesson_id": id, "date": date, "status": status, "word_count": wc,
		})
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"lessons": items})
}

// ---- GET /lessons/{id}：全量（§6.2 示例结构）----

func (h *Handler) Get(w http.ResponseWriter, r *http.Request) {
	userID := auth.FromContext(r.Context())
	lessonID := chi.URLParam(r, "id")

	// 课程归属校验
	var lessonDate, status string
	var wc *int
	err := h.pool.QueryRow(r.Context(), `
		SELECT lesson_date::text, status, word_count
		FROM daily_lesson WHERE id = $1 AND user_id = $2`, lessonID, userID).
		Scan(&lessonDate, &status, &wc)
	if err != nil {
		httpx.Err(w, httpx.NotFound("课程不存在或不属于当前用户"))
		return
	}

	// 词单（含课程内快照状态）
	words := []map[string]any{}
	wrows, err := h.pool.Query(r.Context(), `
		SELECT w.id, w.surface, w.reading, w.pos, w.meaning, w.mastery,
		       lw.is_key, lw.occurrence_count, lw.skipped, lw.skip_reason
		FROM lesson_word lw JOIN word w ON w.id = lw.word_id
		WHERE lw.lesson_id = $1
		ORDER BY w.created_at`, lessonID)
	if err != nil {
		httpx.Err(w, httpx.Internal(err.Error()))
		return
	}
	for wrows.Next() {
		var id, surface, mastery string
		var reading, pos, meaning, skipReason *string
		var isKey bool
		var occur int
		var skipped bool
		if err := wrows.Scan(&id, &surface, &reading, &pos, &meaning, &mastery,
			&isKey, &occur, &skipped, &skipReason); err != nil {
			httpx.Err(w, httpx.Internal(err.Error()))
			return
		}
		words = append(words, map[string]any{
			"id": id, "surface": surface, "reading": reading, "pos": pos,
			"meaning": meaning, "mastery": mastery, "is_key": isKey,
			"occurrences": occur, "skipped": skipped, "skip_reason": skipReason,
		})
	}
	wrows.Close()

	// 材料 + 句子 + 音频（一次取回，代码内组装树）
	type sentenceRow struct {
		MatID       string
		MatType     string
		Scene       *string
		Title       *string
		RoleA       *string
		RoleB       *string
		Sort        int
		SentID      string
		Speaker     *string
		Text        string
		Kana        *string
		Translation *string
		ParaNo      int
		SortOrder   int
		Ruby        []byte
	}
	srows, err := h.pool.Query(r.Context(), `
		SELECT m.id, m.type, m.scene, m.title, m.role_a, m.role_b, m.sort_order,
		       s.id, s.speaker, s.text, s.kana, s.translation, s.para_no, s.sort_order,
		       s.ruby_tokens
		FROM material m
		JOIN sentence s ON s.material_id = m.id
		WHERE m.lesson_id = $1
		ORDER BY m.sort_order, s.sort_order`, lessonID)
	if err != nil {
		httpx.Err(w, httpx.Internal(err.Error()))
		return
	}
	defer srows.Close()

	type materialOut struct {
		ID       string          `json:"id"`
		Type     string          `json:"type"`
		Scene    *string         `json:"scene"`
		Title    *string         `json:"title"`
		RoleA    *string         `json:"role_a"`
		RoleB    *string         `json:"role_b"`
		Sort     int             `json:"sort"`
		Sentences []map[string]any `json:"sentences"`
	}
	mats := []*materialOut{}
	matIndex := map[string]*materialOut{}
	sentIDs := make([]string, 0, 32)
	for srows.Next() {
		var sr sentenceRow
		if err := srows.Scan(&sr.MatID, &sr.MatType, &sr.Scene, &sr.Title, &sr.RoleA, &sr.RoleB,
			&sr.Sort, &sr.SentID, &sr.Speaker, &sr.Text, &sr.Kana, &sr.Translation,
			&sr.ParaNo, &sr.SortOrder, &sr.Ruby); err != nil {
			httpx.Err(w, httpx.Internal(err.Error()))
			return
		}
		m, ok := matIndex[sr.MatID]
		if !ok {
			m = &materialOut{ID: sr.MatID, Type: sr.MatType, Scene: sr.Scene, Title: sr.Title,
				RoleA: sr.RoleA, RoleB: sr.RoleB, Sort: sr.Sort, Sentences: []map[string]any{}}
			matIndex[sr.MatID] = m
			mats = append(mats, m)
		}
		sentIDs = append(sentIDs, sr.SentID)
		m.Sentences = append(m.Sentences, map[string]any{
			"id": sr.SentID, "sort": sr.SortOrder, "speaker": sr.Speaker, "text": sr.Text,
			"kana": sr.Kana, "ruby_tokens": json.RawMessage(sr.Ruby),
			"translation": sr.Translation, "para_no": sr.ParaNo,
			"audios": []any{}, // 音频元数据由 M1-5 的 /audio/{id}/file 关联，此处先置空
		})
	}

	// 音频元数据按句子分组填充（§6.2：audios[].url 为 /audio/{id}/file）
	if len(sentIDs) > 0 {
		audioRows, aerr := h.pool.Query(r.Context(), `
			SELECT id, sentence_id, voice, rate, style, duration_ms, size_bytes
			FROM audio_asset WHERE sentence_id = ANY($1) AND status = 'ready'`, sentIDs)
		if aerr != nil {
			httpx.Err(w, httpx.Internal(aerr.Error()))
			return
		}
		audiosBySent := map[string][]map[string]any{}
		for audioRows.Next() {
			var aid, sid, voice string
			var rate float64
			var style *string
			var dur, size *int
			if err := audioRows.Scan(&aid, &sid, &voice, &rate, &style, &dur, &size); err != nil {
				httpx.Err(w, httpx.Internal(err.Error()))
				return
			}
			audiosBySent[sid] = append(audiosBySent[sid], map[string]any{
				"id": aid, "voice": voice, "rate": rate, "style": style,
				"duration_ms": dur, "size_bytes": size,
				"url": "/api/v1/audio/" + aid + "/file",
			})
		}
		audioRows.Close()
		for _, m := range mats {
			for _, s := range m.Sentences {
				if a, ok := audiosBySent[s["id"].(string)]; ok {
					s["audios"] = a
				}
			}
		}
	}

	httpx.WriteJSON(w, http.StatusOK, map[string]any{
		"lesson":    map[string]any{"id": lessonID, "date": lessonDate, "status": status, "word_count": wc},
		"words":     words,
		"materials": mats,
	})
}
