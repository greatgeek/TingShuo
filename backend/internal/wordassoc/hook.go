// 记忆钩子：为难词按需生成一句助记联想 + 汉字拆解，按词缓存。
// 只对错满 2 次的词自动生成（?auto=1），其余由用户点按生成；不参与背词排期。
package wordassoc

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/greatgeek/TingShuo/backend/internal/auth"
	"github.com/greatgeek/TingShuo/backend/internal/httpx"
	"github.com/greatgeek/TingShuo/backend/pkg/llmclient"
	"github.com/greatgeek/TingShuo/backend/pkg/ulid"
)

var (
	hookLim   = httpx.NewLimiter(5, 10)  // 每用户 5/分钟（仅缓存未命中时计）
	hookDaily = httpx.NewDailyQuota(100) // 每用户 100/天
)

type hookKanji struct {
	Char    string `json:"char"`
	Reading string `json:"reading"`
	Hint    string `json:"hint"`
}

type hookPayload struct {
	Mnemonic  string      `json:"mnemonic"`
	Structure string      `json:"structure"`
	Kanji     []hookKanji `json:"kanji"`
}

// Hook GET /words/{id}/hook?auto=1 —— 缓存优先；auto=1 且错次不足 2 时只返回 available=false。
func (h *Handler) Hook(w http.ResponseWriter, r *http.Request) {
	userID := auth.FromContext(r.Context())
	wordID := chi.URLParam(r, "id")

	var surface, reading, pos, meaning string
	err := h.pool.QueryRow(r.Context(), `
		SELECT surface, COALESCE(reading, ''), COALESCE(pos, ''), COALESCE(meaning, '')
		FROM word WHERE id = $1 AND user_id = $2`, wordID, userID).
		Scan(&surface, &reading, &pos, &meaning)
	if err != nil {
		httpx.Err(w, httpx.NotFound("生词不存在或不属于当前用户"))
		return
	}

	if p, ok := h.loadHook(r.Context(), wordID); ok {
		writeHook(w, wordID, surface, p, true, 0)
		return
	}

	var errCount int
	_ = h.pool.QueryRow(r.Context(),
		`SELECT COALESCE(error_count, 0) FROM word_review_item WHERE word_id = $1`,
		wordID).Scan(&errCount)
	if r.URL.Query().Get("auto") == "1" && errCount < 2 {
		httpx.WriteJSON(w, http.StatusOK, map[string]any{
			"word_id": wordID, "available": false, "error_count": errCount,
		})
		return
	}
	if h.client == nil || os.Getenv("LLM_API_KEY") == "" || os.Getenv("LLM_BASE_URL") == "" {
		httpx.Err(w, httpx.NewError(http.StatusServiceUnavailable, "HOOK_UNAVAILABLE", "记忆钩子服务未配置"))
		return
	}
	if !hookLim.Allow(userID) || !hookDaily.Allow(userID) {
		httpx.Err(w, httpx.NewError(http.StatusTooManyRequests, "QUOTA", "记忆钩子请求过于频繁，请稍后再试（每日上限 100 次）"))
		return
	}

	start := time.Now()
	raw, pt, ct, err := h.client.ChatJSONNoThinking(r.Context(),
		hookPrompt(surface, reading, pos, meaning, h.anchors(r.Context(), wordID)))
	if err != nil {
		httpx.Err(w, httpx.BadGateway("记忆钩子生成失败: "+err.Error()))
		return
	}
	var p hookPayload
	if err := json.Unmarshal([]byte(llmclient.ExtractJSON(raw)), &p); err != nil {
		httpx.Err(w, httpx.BadGateway("记忆钩子解析失败"))
		return
	}
	if err := sanitizeHook(&p, surface, reading); err != nil {
		httpx.Err(w, httpx.BadGateway("记忆钩子内容不合格，请再试一次"))
		return
	}
	if b, err := json.Marshal(p); err == nil {
		_, _ = h.pool.Exec(r.Context(), `
			INSERT INTO word_hook (word_id, payload, model)
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

	writeHook(w, wordID, surface, p, false, errCount)
}

func writeHook(w http.ResponseWriter, wordID, surface string, p hookPayload, cached bool, errCount int) {
	httpx.WriteJSON(w, http.StatusOK, map[string]any{
		"word_id": wordID, "surface": surface, "available": true,
		"mnemonic": p.Mnemonic, "structure": p.Structure, "kanji": p.Kanji,
		"cached": cached, "error_count": errCount,
		"notice": "助记联想，非严格词源",
	})
}

func (h *Handler) loadHook(ctx context.Context, wordID string) (hookPayload, bool) {
	var raw []byte
	if err := h.pool.QueryRow(ctx,
		`SELECT payload FROM word_hook WHERE word_id = $1`, wordID).Scan(&raw); err != nil {
		return hookPayload{}, false
	}
	var p hookPayload
	if json.Unmarshal(raw, &p) != nil || p.Mnemonic == "" {
		return hookPayload{}, false
	}
	if p.Kanji == nil {
		p.Kanji = []hookKanji{}
	}
	return p, true
}

// anchors 从已缓存的联想里取近义/反义各两条，给钩子提供语义参照；
// 没缓存就空着，避免为了钩子额外再花一次联想调用。
func (h *Handler) anchors(ctx context.Context, wordID string) string {
	var raw []byte
	if err := h.pool.QueryRow(ctx,
		`SELECT payload FROM word_association WHERE word_id = $1`, wordID).Scan(&raw); err != nil {
		return ""
	}
	var g assocGroups
	if json.Unmarshal(raw, &g) != nil {
		return ""
	}
	pick := func(items []assocItem, n int) []string {
		out := []string{}
		for _, it := range items {
			if it.Surface == "" {
				continue
			}
			s := it.Surface
			if it.Meaning != "" {
				s += "（" + it.Meaning + "）"
			}
			out = append(out, s)
			if len(out) >= n {
				break
			}
		}
		return out
	}
	syn := pick(g.Synonyms, 2)
	ant := pick(g.Antonyms, 2)
	lines := []string{}
	if len(syn) > 0 {
		lines = append(lines, "近义词："+strings.Join(syn, "、"))
	}
	if len(ant) > 0 {
		lines = append(lines, "反义词："+strings.Join(ant, "、"))
	}
	return strings.Join(lines, "\n")
}

func hookPrompt(surface, reading, pos, meaning, anchors string) string {
	head := surface
	if reading != "" {
		head += "（" + reading + "）"
	}
	if pos != "" {
		head += " " + pos
	}
	if meaning != "" {
		head += " " + meaning
	}
	ref := ""
	if anchors != "" {
		ref = "\n可参考的语义关系：\n" + anchors
	}
	return fmt.Sprintf(`你是日语学习助手，为中文学习者给一个日语词做记忆钩子。
目标词：%s%s
要求：
1. mnemonic：一句中文助记，必须出现这个日语词本身；把词义和汉字画面/读音联系起来，10-60 字，不要空话；
2. structure：可选，词的构成提示（如「漢字A＋漢字B」「动词て形＋しまう」），没有就给空字符串；
3. kanji：逐个拆解词里的汉字，最多 4 个；每项 char=汉字、reading=该字在此词里的假名读音、hint=该字在此词里的意思（≤20 字）；没有汉字就给空数组；
4. 这是助记联想，不要声称是严格词源，不要编造典故。
只输出 JSON，不要解释：
{"mnemonic":"...","structure":"...","kanji":[{"char":"...","reading":"...","hint":"..."}]}`, head, ref)
}

// sanitizeHook 校验并裁齐模型输出：助记必须含目标词（或读音），汉字必须是词里的字。
func sanitizeHook(p *hookPayload, surface, reading string) error {
	p.Mnemonic = strings.TrimSpace(p.Mnemonic)
	p.Structure = truncRunes(strings.TrimSpace(p.Structure), 40)
	n := runeLen(p.Mnemonic)
	if n < 4 || n > 100 {
		return fmt.Errorf("助记长度 %d 不合格", n)
	}
	if !strings.Contains(p.Mnemonic, surface) &&
		(reading == "" || !strings.Contains(p.Mnemonic, reading)) {
		return fmt.Errorf("助记未包含目标词")
	}
	seen := map[rune]bool{}
	out := []hookKanji{}
	for _, k := range p.Kanji {
		cs := []rune(strings.TrimSpace(k.Char))
		if len(cs) != 1 || !strings.ContainsRune(surface, cs[0]) || seen[cs[0]] {
			continue
		}
		rd := strings.TrimSpace(k.Reading)
		if rd == "" || !isKanaReading(rd) {
			continue
		}
		hint := truncRunes(strings.TrimSpace(k.Hint), 24)
		if hint == "" {
			continue
		}
		seen[cs[0]] = true
		out = append(out, hookKanji{Char: string(cs[0]), Reading: rd, Hint: hint})
		if len(out) >= 4 {
			break
		}
	}
	p.Kanji = out
	return nil
}
