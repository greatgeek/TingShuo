// Package wordassoc 生词「联想」：按词请求大模型给出近义词/反义词/联想词，
// 结果按词缓存（word_association），联想词可一键加入生词本当前目录。
package wordassoc

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/greatgeek/TingShuo/backend/internal/auth"
	"github.com/greatgeek/TingShuo/backend/internal/generate"
	"github.com/greatgeek/TingShuo/backend/internal/httpx"
	"github.com/greatgeek/TingShuo/backend/pkg/llmclient"
	"github.com/greatgeek/TingShuo/backend/pkg/ulid"
)

var (
	assocLim   = httpx.NewLimiter(5, 10)  // 每用户 5/分钟（仅缓存未命中时计）
	assocDaily = httpx.NewDailyQuota(150) // 每用户 150/天（LLM 成本护栏）
)

type Handler struct {
	pool   *pgxpool.Pool
	client *llmclient.Client
}

func NewHandler(pool *pgxpool.Pool) *Handler {
	return &Handler{
		pool: pool,
		client: llmclient.New(
			os.Getenv("LLM_BASE_URL"), os.Getenv("LLM_API_KEY"), os.Getenv("LLM_MODEL")),
	}
}

// assocItem 联想词：surface 词形 / reading 读音 / meaning 中文释义。
type assocItem struct {
	Surface string `json:"surface"`
	Reading string `json:"reading"`
	Meaning string `json:"meaning"`
}

type assocGroups struct {
	Synonyms []assocItem `json:"synonyms"`
	Antonyms []assocItem `json:"antonyms"`
	Related  []assocItem `json:"related"`
}

// Get GET /words/{id}/association：缓存优先；未命中才请求大模型并落缓存。
func (h *Handler) Get(w http.ResponseWriter, r *http.Request) {
	userID := auth.FromContext(r.Context())
	wordID := chi.URLParam(r, "id")

	var surface string
	var reading, pos, meaning *string
	err := h.pool.QueryRow(r.Context(), `
		SELECT surface, reading, pos, meaning FROM word WHERE id = $1 AND user_id = $2`,
		wordID, userID).Scan(&surface, &reading, &pos, &meaning)
	if err != nil {
		httpx.Err(w, httpx.NotFound("生词不存在或不属于当前用户"))
		return
	}

	if g, ok := h.loadCache(r.Context(), wordID); ok {
		httpx.WriteJSON(w, http.StatusOK, map[string]any{
			"word_id": wordID, "surface": surface, "groups": g, "cached": true,
		})
		return
	}

	if os.Getenv("LLM_API_KEY") == "" || os.Getenv("LLM_BASE_URL") == "" {
		httpx.Err(w, httpx.NewError(http.StatusServiceUnavailable, "LLM_UNAVAILABLE", "联想服务未配置"))
		return
	}
	if !assocLim.Allow(userID) || !assocDaily.Allow(userID) {
		httpx.Err(w, httpx.NewError(http.StatusTooManyRequests, "QUOTA", "联想请求过于频繁，请稍后再试（每日上限 150 次）"))
		return
	}

	start := time.Now()
	g, pt, ct, err := h.fetch(r.Context(), surface, deref(reading), deref(pos), deref(meaning))
	if err != nil {
		httpx.Err(w, httpx.BadGateway("联想生成失败: "+err.Error()))
		return
	}
	// 缓存写入尽力而为：失败只影响下次重新生成
	if b, err := json.Marshal(g); err == nil {
		_, _ = h.pool.Exec(r.Context(), `
			INSERT INTO word_association (word_id, payload, model)
			VALUES ($1, $2, $3)
			ON CONFLICT (word_id) DO UPDATE
			  SET payload = EXCLUDED.payload, model = EXCLUDED.model, created_at = now()`,
			wordID, b, os.Getenv("LLM_MODEL"))
	}
	_, _ = h.pool.Exec(r.Context(), `
		INSERT INTO llm_call_log (id, user_id, lesson_id, model, prompt_tokens, output_tokens, duration_ms)
		VALUES ($1, $2, NULL, $3, $4, $5, $6)`,
		ulid.MustNew(), userID, os.Getenv("LLM_MODEL"),
		pt, ct, time.Since(start).Milliseconds())

	httpx.WriteJSON(w, http.StatusOK, map[string]any{
		"word_id": wordID, "surface": surface, "groups": g, "cached": false,
	})
}

func (h *Handler) loadCache(ctx context.Context, wordID string) (assocGroups, bool) {
	var raw []byte
	if err := h.pool.QueryRow(ctx,
		`SELECT payload FROM word_association WHERE word_id = $1`, wordID).Scan(&raw); err != nil {
		return assocGroups{}, false
	}
	var g assocGroups
	if json.Unmarshal(raw, &g) != nil {
		return assocGroups{}, false
	}
	if g.Synonyms == nil {
		g.Synonyms = []assocItem{}
	}
	if g.Antonyms == nil {
		g.Antonyms = []assocItem{}
	}
	if g.Related == nil {
		g.Related = []assocItem{}
	}
	// 旧缓存自愈：早期大模型读音偶有漏读（家電店→かいてん），按词典校正后回写，
	// 免清缓存也能立刻纠正注音与发音。
	if correctGroupReadings(&g) {
		if b, err := json.Marshal(g); err == nil {
			_, _ = h.pool.Exec(ctx,
				`UPDATE word_association SET payload = $2 WHERE word_id = $1`, wordID, b)
		}
	}
	return g, true
}

// correctReading 用词典修正大模型漏读的读音（家電店 かいてん→かでんてん）；
// 词典无法确证时保留原读音。
func correctReading(surface, reading string) string {
	return generate.CorrectReading(surface, reading)
}

// correctGroupReadings 校正三组词条的读音，返回是否有改动。
func correctGroupReadings(g *assocGroups) bool {
	changed := false
	for _, group := range []*[]assocItem{&g.Synonyms, &g.Antonyms, &g.Related} {
		for i := range *group {
			it := &(*group)[i]
			if r := correctReading(it.Surface, it.Reading); r != it.Reading {
				it.Reading = r
				changed = true
			}
		}
	}
	return changed
}

func (h *Handler) fetch(ctx context.Context, surface, reading, pos, meaning string) (assocGroups, int, int, error) {
	info := surface
	if reading != "" {
		info += "（" + reading + "）"
	}
	if pos != "" {
		info += " " + pos
	}
	if meaning != "" {
		info += " " + meaning
	}
	prompt := fmt.Sprintf(`你是日语学习助手。给定一个日语词，请给出与它相关的日语词，帮助中文学习者建立词汇联想。
目标词：%s
要求：
1. synonyms：近义词（意思相近的日语词）；
2. antonyms：反义词（意思相反的日语词），没有明确反义词时返回空数组；
3. related：联想词（常见搭配、同一场景或主题、常一起出现的日语词）；
4. 每组最多 5 个；词须为日语常用词（动词给辞书形），排除目标词本身及其活用形；
5. 词形必须是日语标准表记（汉字或假名），不要输出繁体中文或中文词语（例如「考試」应写「試験」）；
6. 每个词给出平假名读音 reading（片假名词可用片假名）与不超过 12 字的中文释义 meaning。
只输出 JSON，不要解释：
{"synonyms":[{"surface":"...","reading":"...","meaning":"..."}],"antonyms":[...],"related":[...]}`, info)

	raw, pt, ct, err := h.client.ChatJSONNoThinking(ctx, prompt)
	if err != nil {
		return assocGroups{}, 0, 0, err
	}
	var parsed assocGroups
	if err := json.Unmarshal([]byte(llmclient.ExtractJSON(raw)), &parsed); err != nil {
		return assocGroups{}, 0, 0, fmt.Errorf("联想结果解析失败: %w", err)
	}
	seen := map[string]bool{}
	g := assocGroups{
		Synonyms: sanitizeItems(parsed.Synonyms, surface, seen, 5),
		Antonyms: sanitizeItems(parsed.Antonyms, surface, seen, 4),
		Related:  sanitizeItems(parsed.Related, surface, seen, 6),
	}
	// 二道校验：模型偶尔生造词（如「研究所室」）或标错读音，逐条复核后再展示
	vpt, vct := h.verify(ctx, surface, &g)
	pt += vpt
	ct += vct
	if len(g.Synonyms)+len(g.Antonyms)+len(g.Related) == 0 {
		return g, pt, ct, errors.New("AI 未给出可用的联想词")
	}
	return g, pt, ct, nil
}

// verify 复核候选词是否真实存在、读音正确且与目标相关；只剔除明确有问题的条目。
// 校验调用失败时保留首轮结果（宁少勿错 vs 可用性权衡：失败不阻塞展示）。
func (h *Handler) verify(ctx context.Context, source string, g *assocGroups) (int, int) {
	starts := map[string]int{
		"synonyms": 0,
		"antonyms": len(g.Synonyms),
		"related":  len(g.Synonyms) + len(g.Antonyms),
	}
	var lines []string
	add := func(items []assocItem) {
		for _, it := range items {
			lines = append(lines, fmt.Sprintf("%d. %s（%s）%s", len(lines), it.Surface, it.Reading, it.Meaning))
		}
	}
	add(g.Synonyms)
	add(g.Antonyms)
	add(g.Related)
	if len(lines) == 0 {
		return 0, 0
	}
	prompt := fmt.Sprintf(`判断下面每个候选词能否作为日语学习材料。目标词：%s
逐项检查：①是否为真实存在的日语词（排除生造词、拼写错误）；②词形是否为日语表记（排除中文词）；③读音是否符合该词、且逐字覆盖词形不得漏读（如 家電店→かでんてん，不是かいてん）；④是否与目标词确实相关。
只输出存在问题的序号 JSON 数组（如 [2,5]）；都没问题输出 []，不确定的不要标，不要解释。
%s`, source, strings.Join(lines, "\n"))
	raw, pt, ct, err := h.client.ChatJSONNoThinking(ctx, prompt)
	if err != nil {
		return 0, 0
	}
	bad := map[int]bool{}
	for _, n := range parseIntArray(raw) {
		if n >= 0 && n < len(lines) {
			bad[n] = true
		}
	}
	if len(bad) == 0 {
		return pt, ct
	}
	keep := func(group string, items []assocItem) []assocItem {
		out := []assocItem{}
		for i, it := range items {
			if !bad[starts[group]+i] {
				out = append(out, it)
			}
		}
		return out
	}
	g.Synonyms = keep("synonyms", g.Synonyms)
	g.Antonyms = keep("antonyms", g.Antonyms)
	g.Related = keep("related", g.Related)
	return pt, ct
}

// parseIntArray 从模型回复中提取序号数组（容忍前后混入说明文字）。
func parseIntArray(s string) []int {
	if i := strings.Index(s, "["); i >= 0 {
		if j := strings.LastIndex(s, "]"); j > i {
			s = s[i : j+1]
		}
	}
	var arr []int
	if json.Unmarshal([]byte(s), &arr) != nil {
		return nil
	}
	return arr
}

// sanitizeItems 丢弃畸形条目（非日语、过长、缺读音的汉字词），跨组去重并截断数量。
func sanitizeItems(src []assocItem, sourceSurface string, seen map[string]bool, max int) []assocItem {
	out := []assocItem{}
	for _, it := range src {
		s := strings.TrimSpace(it.Surface)
		r := strings.TrimSpace(it.Reading)
		m := strings.TrimSpace(it.Meaning)
		if s == "" || s == sourceSurface || runeLen(s) > 20 || !hasJapanese(s) {
			continue
		}
		if r == "" {
			if hasKanji(s) {
				continue
			}
			r = s
		}
		if !isKanaReading(r) {
			continue
		}
		r = correctReading(s, r)
		if seen[s] {
			continue
		}
		seen[s] = true
		out = append(out, assocItem{Surface: s, Reading: truncRunes(r, 50), Meaning: truncRunes(m, 40)})
		if len(out) >= max {
			break
		}
	}
	return out
}

// AddWord POST /notebooks/dir/{dir_id}/words：把联想词加入当前目录（同词幂等）。
func (h *Handler) AddWord(w http.ResponseWriter, r *http.Request) {
	userID := auth.FromContext(r.Context())
	dirID := chi.URLParam(r, "dir_id")
	var req struct {
		Surface string `json:"surface"`
		Reading string `json:"reading"`
		Meaning string `json:"meaning"`
		Pos     string `json:"pos"`
	}
	if e := httpx.Decode(r, &req); e != nil {
		httpx.Err(w, e)
		return
	}
	req.Surface = strings.TrimSpace(req.Surface)
	req.Reading = strings.TrimSpace(req.Reading)
	req.Meaning = strings.TrimSpace(req.Meaning)
	req.Pos = strings.TrimSpace(req.Pos)
	if req.Surface == "" || runeLen(req.Surface) > 20 || !hasJapanese(req.Surface) || runeLen(req.Meaning) > 200 {
		httpx.Err(w, httpx.BadRequest("BAD_WORD", "联想词格式不正确"))
		return
	}
	if req.Reading == "" || runeLen(req.Reading) > 50 || !isKanaReading(req.Reading) {
		httpx.Err(w, httpx.BadRequest("BAD_WORD", "联想词缺少读音"))
		return
	}
	req.Reading = correctReading(req.Surface, req.Reading)
	req.Meaning = truncRunes(req.Meaning, 200)
	req.Pos = truncRunes(req.Pos, 20)

	var nbID string
	err := h.pool.QueryRow(r.Context(), `
		SELECT d.notebook_id FROM notebook_dir d
		JOIN notebook nb ON nb.id = d.notebook_id
		WHERE d.id = $1 AND nb.user_id = $2`, dirID, userID).Scan(&nbID)
	if err != nil {
		httpx.Err(w, httpx.NotFound("目录不存在或不属于当前用户"))
		return
	}

	tx, err := h.pool.Begin(r.Context())
	if err != nil {
		httpx.Err(w, httpx.Internal(err.Error()))
		return
	}
	defer tx.Rollback(r.Context())

	var wordID string
	err = tx.QueryRow(r.Context(), `
		INSERT INTO word (id, user_id, surface, reading, pos, meaning)
		VALUES ($1, $2, $3, $4, NULLIF($5, ''), NULLIF($6, ''))
		ON CONFLICT (user_id, surface, reading) DO UPDATE
		  SET meaning = CASE WHEN word.meaning IS NULL OR word.meaning = ''
		                     THEN EXCLUDED.meaning ELSE word.meaning END,
		      pos = COALESCE(word.pos, EXCLUDED.pos),
		      updated_at = now()
		RETURNING id`,
		ulid.MustNew(), userID, req.Surface, req.Reading, req.Pos, req.Meaning).Scan(&wordID)
	if err != nil {
		httpx.Err(w, httpx.Internal(fmt.Sprintf("词入库失败: %v", err)))
		return
	}

	var linkID string
	added := true
	err = tx.QueryRow(r.Context(), `
		INSERT INTO notebook_word (id, notebook_id, dir_id, word_id, sort_order)
		SELECT $1, $2, $3, $4, COALESCE(MAX(sort_order), -1) + 1
		FROM notebook_word WHERE dir_id = $3
		ON CONFLICT (dir_id, word_id) DO NOTHING
		RETURNING id`,
		ulid.MustNew(), nbID, dirID, wordID).Scan(&linkID)
	if errors.Is(err, pgx.ErrNoRows) {
		added = false
		if err := tx.QueryRow(r.Context(),
			`SELECT id FROM notebook_word WHERE dir_id = $1 AND word_id = $2`,
			dirID, wordID).Scan(&linkID); err != nil {
			httpx.Err(w, httpx.Internal(err.Error()))
			return
		}
	} else if err != nil {
		httpx.Err(w, httpx.Internal(fmt.Sprintf("加入目录失败: %v", err)))
		return
	}
	if err := tx.Commit(r.Context()); err != nil {
		httpx.Err(w, httpx.Internal(err.Error()))
		return
	}

	httpx.WriteJSON(w, http.StatusOK, map[string]any{
		"link_id": linkID, "word_id": wordID, "surface": req.Surface,
		"reading": req.Reading, "meaning": req.Meaning, "pos": req.Pos, "added": added,
	})
}

// RemoveWord DELETE /notebooks/dir/{dir_id}/words/{word_id}：取消加入。
// 先解除与当前目录的关联；若该词已不在任何目录，且没有背词卡/例句/课文词等
// 依赖（外键无级联），则连词一起删掉——联想误加的彻底撤销。
// 有学习记录或仍在别的目录时只解除本目录关联，保留词与记录。
func (h *Handler) RemoveWord(w http.ResponseWriter, r *http.Request) {
	userID := auth.FromContext(r.Context())
	dirID := chi.URLParam(r, "dir_id")
	wordID := chi.URLParam(r, "word_id")

	var ok bool
	err := h.pool.QueryRow(r.Context(), `
		SELECT EXISTS (
			SELECT 1 FROM notebook_dir d
			JOIN notebook nb ON nb.id = d.notebook_id
			WHERE d.id = $1 AND nb.user_id = $2)`, dirID, userID).Scan(&ok)
	if err != nil || !ok {
		httpx.Err(w, httpx.NotFound("目录不存在或不属于当前用户"))
		return
	}

	tx, err := h.pool.Begin(r.Context())
	if err != nil {
		httpx.Err(w, httpx.Internal(err.Error()))
		return
	}
	defer tx.Rollback(r.Context())

	tag, err := tx.Exec(r.Context(),
		`DELETE FROM notebook_word WHERE dir_id = $1 AND word_id = $2`, dirID, wordID)
	if err != nil {
		httpx.Err(w, httpx.Internal(err.Error()))
		return
	}
	removed := tag.RowsAffected() > 0

	purged := false
	if removed {
		// 保存点兜底：词仍被背词卡/例句等引用时外键会拒绝删除，
		// 回滚到保存点保留词，目录关联照常提交。
		if _, err := tx.Exec(r.Context(), `SAVEPOINT purge_word`); err != nil {
			httpx.Err(w, httpx.Internal(err.Error()))
			return
		}
		tag, err = tx.Exec(r.Context(), `
			DELETE FROM word
			WHERE id = $1 AND user_id = $2
			  AND NOT EXISTS (SELECT 1 FROM notebook_word WHERE word_id = $1)`,
			wordID, userID)
		if err != nil {
			if _, e := tx.Exec(r.Context(), `ROLLBACK TO SAVEPOINT purge_word`); e != nil {
				httpx.Err(w, httpx.Internal(e.Error()))
				return
			}
		} else {
			purged = tag.RowsAffected() > 0
			if _, err := tx.Exec(r.Context(), `RELEASE SAVEPOINT purge_word`); err != nil {
				httpx.Err(w, httpx.Internal(err.Error()))
				return
			}
		}
	}
	if err := tx.Commit(r.Context()); err != nil {
		httpx.Err(w, httpx.Internal(err.Error()))
		return
	}

	httpx.WriteJSON(w, http.StatusOK, map[string]any{
		"removed": removed, "purged": purged, "word_id": wordID,
	})
}

func deref(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}

func runeLen(s string) int { return len([]rune(s)) }

func truncRunes(s string, n int) string {
	r := []rune(s)
	if len(r) > n {
		return string(r[:n])
	}
	return s
}

func hasJapanese(s string) bool {
	for _, r := range s {
		if (r >= 0x3040 && r <= 0x30FF) || (r >= 0x4E00 && r <= 0x9FFF) ||
			r == 0x3005 || r == 0x3006 {
			return true
		}
	}
	return false
}

func hasKanji(s string) bool {
	for _, r := range s {
		if (r >= 0x4E00 && r <= 0x9FFF) || r == 0x3005 {
			return true
		}
	}
	return false
}

// isKanaReading 读音只能是假名（含长音、中点、空格），挡掉模型混入的拉丁字母/汉字。
func isKanaReading(s string) bool {
	hasKana := false
	for _, r := range s {
		switch {
		case r >= 0x3041 && r <= 0x3096: // 平假名
			hasKana = true
		case r >= 0x30A1 && r <= 0x30FA: // 片假名
			hasKana = true
		case r == 0x30FC || r == 0x30FD || r == 0x30FE: // ー ヽ ヾ
		case r == ' ' || r == '　' || r == '・':
		default:
			return false
		}
	}
	return hasKana
}
