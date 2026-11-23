// 词条详情：JMdict 词条 + LLM 中文释义与例句（按词条缓存）。
// 常用词预翻只写中文义项；例句在首次打开详情时按需生成并合并进同一份缓存。
package dict

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"

	"github.com/greatgeek/TingShuo/backend/internal/auth"
	"github.com/greatgeek/TingShuo/backend/internal/generate"
	"github.com/greatgeek/TingShuo/backend/internal/httpx"
	"github.com/greatgeek/TingShuo/backend/pkg/llmclient"
	"github.com/greatgeek/TingShuo/backend/pkg/ulid"
)

var (
	detailLim   = httpx.NewLimiter(20, 10) // 每用户 20/分钟（仅详情未命中缓存时计）
	detailDaily = httpx.NewDailyQuota(300) // 每用户 300/天
	fallbackLim = httpx.NewLimiter(5, 10)  // AI 兜底词条：每用户 5/分钟
	fbDaily     = httpx.NewDailyQuota(100) // 每用户 100/天
)

type zhSense struct {
	Pos string `json:"pos"`
	Zh  zhList `json:"zh"`
}

// zhList 容忍模型把 zh 输出成单个字符串而不是数组（预翻批次曾因此整批解析失败）
type zhList []string

func (l *zhList) UnmarshalJSON(b []byte) error {
	b = bytes.TrimSpace(b)
	if len(b) > 0 && b[0] == '"' {
		var s string
		if err := json.Unmarshal(b, &s); err != nil {
			return err
		}
		*l = zhList{s}
		return nil
	}
	var arr []string
	if err := json.Unmarshal(b, &arr); err != nil {
		return err
	}
	*l = zhList(arr)
	return nil
}

type zhExample struct {
	Jp   string  `json:"jp"`
	Zh   string  `json:"zh"`
	Ruby [][]any `json:"ruby,omitempty"`
}

type zhContent struct {
	Senses   []zhSense   `json:"senses"`
	Examples []zhExample `json:"examples"`
}

// Entry GET /dict/entries/{entry_id}
func (h *Handler) Entry(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(chi.URLParam(r, "entry_id"), 10, 64)
	if err != nil || id <= 0 {
		httpx.Err(w, httpx.BadRequest("BAD_ENTRY", "词条编号不正确"))
		return
	}
	ctx := r.Context()

	var common bool
	var sensesRaw []byte
	var zhRaw []byte
	var srcHash string
	var formsRaw []byte
	err = h.pool.QueryRow(ctx, `
		SELECT e.common, e.senses, z.payload, COALESCE(z.src_hash, ''), `+formsJSON+`
		FROM dict_entry e
		LEFT JOIN dict_zh z ON z.entry_id = e.id
		WHERE e.id = $1`, id).Scan(&common, &sensesRaw, &zhRaw, &srcHash, &formsRaw)
	if err == pgx.ErrNoRows {
		httpx.Err(w, httpx.NotFound("词条不存在"))
		return
	}
	if err != nil {
		httpx.Err(w, httpx.Internal(err.Error()))
		return
	}

	item := buildItem(rawItem{id: id, common: common, senses: sensesRaw, forms: formsRaw}, "exact")
	hash := sensesHash(sensesRaw)

	var zh *zhContent
	cached := false
	if len(zhRaw) > 0 && srcHash == hash {
		var c zhContent
		if json.Unmarshal(zhRaw, &c) == nil {
			cached = len(c.Examples) > 0
			zh = &c
		}
	}

	// 有中文义项但缺例句（预翻产物）：只补例句
	if zh != nil && len(zh.Examples) == 0 {
		if ok := h.allowLLM(ctx, auth.FromContext(ctx), detailLim, detailDaily); ok {
			examples := h.completeExamples(ctx, auth.FromContext(ctx), item, zh.Senses, nil)
			if len(examples) > 0 {
				zh.Examples = examples
				h.saveZh(ctx, id, hash, zh)
			}
		}
	}

	// 完全没有中文内容：一次生成义项 + 例句
	if zh == nil {
		if ok := h.allowLLM(ctx, auth.FromContext(ctx), detailLim, detailDaily); ok {
			c, _, err := h.generateZh(ctx, auth.FromContext(ctx), item, sensesRaw)
			if err == nil {
				zh = c
				h.saveZh(ctx, id, hash, zh)
			}
		}
	}

	httpx.WriteJSON(w, http.StatusOK, map[string]any{
		"entry_id": id,
		"forms":    item.Forms,
		"readings": item.Readings,
		"common":   item.Common,
		"pos":      item.Pos,
		"glosses":  item.Glosses,
		"zh":       zh,
		"cached":   cached,
	})
}

// Fallback GET /dict/fallback?q= —— 词典未收录的日语词由 LLM 兜底生成并缓存。
func (h *Handler) Fallback(w http.ResponseWriter, r *http.Request) {
	q := normalizeQuery(r.URL.Query().Get("q"))
	if q == "" || !hasJapanese(q) || len([]rune(q)) > 20 {
		httpx.Err(w, httpx.BadRequest("BAD_Q", "请输入要生成的日语词"))
		return
	}
	ctx := r.Context()
	userID := auth.FromContext(ctx)

	var cachedRaw []byte
	if err := h.pool.QueryRow(ctx,
		`SELECT payload FROM dict_fallback WHERE q = $1`, q).Scan(&cachedRaw); err == nil {
		var c fallbackPayload
		if json.Unmarshal(cachedRaw, &c) == nil {
			httpx.WriteJSON(w, http.StatusOK, map[string]any{
				"q": q, "valid": true, "surface": q, "reading": c.Reading, "zh": c.Zh, "cached": true,
			})
			return
		}
	}

	if os.Getenv("LLM_API_KEY") == "" || os.Getenv("LLM_BASE_URL") == "" {
		httpx.Err(w, httpx.NewError(http.StatusServiceUnavailable, "LLM_UNAVAILABLE", "AI 词条服务未配置"))
		return
	}
	if !fallbackLim.Allow(userID) || !fbDaily.Allow(userID) {
		httpx.Err(w, httpx.NewError(http.StatusTooManyRequests, "QUOTA", "AI 词条请求过于频繁，请稍后再试（每日上限 100 次）"))
		return
	}

	prompt := "用户查询日语词「" + q + "」，但通用词典未收录（可能是专有名词、新词、口语或拼写错误）。\n" +
		"请判断它是否是真实存在、可解释的日语词或表达。\n" +
		"如果不是日语（中文、英文、随机字母）或明显拼错，只输出 {\"valid\":false}。\n" +
		"如果是：给出读音（平假名；片假名词可用片假名）、中文释义（1-3 个义项）和 3 个地道日语例句（每句 ≤ 35 字，附中文翻译，不要混入中文或英文词，也不要用具体人名、地名）。\n" +
		"只输出 JSON：{\"valid\":true,\"reading\":\"へや\",\"senses\":[{\"pos\":\"名词\",\"zh\":[\"房间\",\"屋子\"]}]," +
		"\"examples\":[{\"jp\":\"...\",\"zh\":\"...\"}]}"
	start := time.Now()
	content, pt, ct, err := h.client.ChatJSONNoThinking(ctx, prompt)
	if err != nil {
		httpx.Err(w, httpx.BadGateway("AI 词条生成失败: "+err.Error()))
		return
	}
	h.logLLM(ctx, userID, pt, ct, time.Since(start))

	var resp struct {
		Valid    bool        `json:"valid"`
		Reading  string      `json:"reading"`
		Senses   []zhSense   `json:"senses"`
		Examples []zhExample `json:"examples"`
	}
	if json.Unmarshal([]byte(llmclient.ExtractJSON(content)), &resp) != nil {
		httpx.Err(w, httpx.BadGateway("AI 词条格式异常"))
		return
	}
	if !resp.Valid {
		httpx.WriteJSON(w, http.StatusOK, map[string]any{"q": q, "valid": false})
		return
	}
	reading := strings.TrimSpace(resp.Reading)
	if reading != "" && !isKana(reading) {
		reading = ""
	}
	if reading == "" && isKana(q) {
		reading = q
	}
	item := Item{Forms: []Form{{Text: q, Kind: "k"}}}
	if reading != "" {
		item.Readings = []string{reading}
	}
	c := zhContent{}
	c.Senses = sanitizeZhSenses(resp.Senses)
	c.Examples = sanitizeExamples(resp.Examples)
	if len(c.Senses) == 0 {
		httpx.WriteJSON(w, http.StatusOK, map[string]any{"q": q, "valid": false})
		return
	}
	c.Examples = h.completeExamples(ctx, userID, item, c.Senses, c.Examples)
	// 读音以例句里实际分词的读法为准，避免与发音不一致
	if r := readingFromExamples(c.Examples, q); r != "" {
		reading = r
	}
	payload, _ := json.Marshal(fallbackPayload{Reading: reading, Zh: c})
	_, _ = h.pool.Exec(ctx, `
		INSERT INTO dict_fallback (q, payload, model) VALUES ($1, $2, $3)
		ON CONFLICT (q) DO UPDATE SET payload = EXCLUDED.payload, model = EXCLUDED.model, created_at = now()`,
		q, payload, os.Getenv("LLM_MODEL"))

	httpx.WriteJSON(w, http.StatusOK, map[string]any{
		"q": q, "valid": true, "surface": q, "reading": reading, "zh": c, "cached": false,
	})
}

type fallbackPayload struct {
	Reading string    `json:"reading"`
	Zh      zhContent `json:"zh"`
}

func (h *Handler) allowLLM(ctx context.Context, userID string, lim *httpx.Limiter, daily *httpx.DailyQuota) bool {
	if os.Getenv("LLM_API_KEY") == "" || os.Getenv("LLM_BASE_URL") == "" {
		return false
	}
	return lim.Allow(userID) && daily.Allow(userID)
}

func (h *Handler) logLLM(ctx context.Context, userID string, pt, ct int, dur time.Duration) {
	_, _ = h.pool.Exec(ctx, `
		INSERT INTO llm_call_log (id, user_id, lesson_id, model, prompt_tokens, output_tokens, duration_ms)
		VALUES ($1, $2, NULL, $3, $4, $5, $6)`,
		ulid.MustNew(), userID, os.Getenv("LLM_MODEL"), pt, ct, dur.Milliseconds())
}

// generateZh 生成完整中文内容：义项按词条英文释义归纳，例句覆盖主要义项。
func (h *Handler) generateZh(ctx context.Context, userID string, item Item, sensesRaw []byte) (*zhContent, int, error) {
	prompt := detailPrompt(item, sensesRaw)
	start := time.Now()
	content, pt, ct, err := h.client.ChatJSONNoThinking(ctx, prompt)
	h.logLLM(ctx, userID, pt, ct, time.Since(start))
	if err != nil {
		return nil, 0, err
	}
	var resp struct {
		Senses   []zhSense   `json:"senses"`
		Examples []zhExample `json:"examples"`
	}
	if json.Unmarshal([]byte(llmclient.ExtractJSON(content)), &resp) != nil {
		return nil, 0, fmt.Errorf("AI 释义格式异常")
	}
	c := &zhContent{
		Senses:   sanitizeZhSenses(resp.Senses),
		Examples: sanitizeExamples(resp.Examples),
	}
	if len(c.Senses) == 0 {
		return nil, 0, fmt.Errorf("AI 未给出可用释义")
	}
	// 模型偶尔少给或不给例句：先补齐，质检后再补一轮
	c.Examples = h.completeExamples(ctx, userID, item, c.Senses, c.Examples)
	return c, pt, nil
}

// completeExamples 补足 3 条例句并做一次质检；质检剔掉后再补一轮并复检。
func (h *Handler) completeExamples(ctx context.Context, userID string, item Item, senses []zhSense, examples []zhExample) []zhExample {
	if os.Getenv("DICT_DEBUG") != "" {
		fmt.Printf("[dict] complete %s in=%d\n", entryTitle(item), len(examples))
	}
	if len(examples) == 0 {
		more, _, _ := h.examplesOnly(ctx, userID, item, senses, 3, nil)
		examples = mergeExamples(examples, more, 3)
	}
	for attempt := 0; attempt < 2 && len(examples) < 3; attempt++ {
		more, _, _ := h.examplesOnly(ctx, userID, item, senses, 3-len(examples), examples)
		examples = mergeExamples(examples, more, 3)
	}
	examples = h.verifyExamples(ctx, userID, item, examples)
	if len(examples) < 3 {
		more, _, _ := h.examplesOnly(ctx, userID, item, senses, 3-len(examples), examples)
		examples = mergeExamples(examples, more, 3)
		examples = h.verifyExamples(ctx, userID, item, examples)
	}
	if os.Getenv("DICT_DEBUG") != "" {
		fmt.Printf("[dict] complete %s out=%d\n", entryTitle(item), len(examples))
	}
	return examples
}

// examplesOnly 预翻词条只有中文义项，首次打开时只补例句。
func (h *Handler) examplesOnly(ctx context.Context, userID string, item Item, senses []zhSense, need int, avoid []zhExample) ([]zhExample, int, error) {
	if need <= 0 {
		return nil, 0, nil
	}
	var b strings.Builder
	b.WriteString("词条：" + entryTitle(item) + "\n已有中文释义：\n")
	for i, s := range senses {
		b.WriteString(fmt.Sprintf("%d. %s：%s\n", i+1, s.Pos, strings.Join(s.Zh, "、")))
	}
	if len(avoid) > 0 {
		b.WriteString("已有例句（不要重复）：\n")
		for _, ex := range avoid {
			b.WriteString("- " + ex.Jp + "\n")
		}
	}
	b.WriteString(fmt.Sprintf("请再给 %d 个自然、常用的日语例句（每句 ≤ 35 字），覆盖主要义项，并附准确中文翻译；必须是地道日语，不要混入英文或中文词，也不要用具体人名、地名。\n", need) +
		"只输出 JSON：{\"examples\":[{\"jp\":\"...\",\"zh\":\"...\"}]}")
	start := time.Now()
	content, pt, ct, err := h.client.ChatJSONNoThinking(ctx, b.String())
	h.logLLM(ctx, userID, pt, ct, time.Since(start))
	if err != nil {
		return nil, 0, err
	}
	var resp struct {
		Examples []zhExample `json:"examples"`
	}
	if json.Unmarshal([]byte(llmclient.ExtractJSON(content)), &resp) != nil {
		return nil, 0, fmt.Errorf("AI 例句格式异常")
	}
	return sanitizeExamples(resp.Examples), pt, nil
}

func detailPrompt(item Item, sensesRaw []byte) string {
	var b strings.Builder
	b.WriteString("你是日语词典编辑。请根据下面 JMdict 词条，为中文母语学习者写简明日中释义与例句。\n")
	b.WriteString("词条：" + entryTitle(item) + "\n英文义项：\n")
	var jm []jmSense
	_ = json.Unmarshal(sensesRaw, &jm)
	n := 0
	for _, s := range jm {
		glosses := []string{}
		for _, g := range s.Gloss {
			glosses = append(glosses, g.Text)
		}
		if len(glosses) == 0 {
			continue
		}
		n++
		label := posHint(s.PartOfSpeech)
		if len(s.Field) > 0 {
			label += "，领域：" + strings.Join(s.Field, "/")
		}
		if len(s.Misc) > 0 {
			label += "，标记：" + strings.Join(s.Misc, "/")
		}
		b.WriteString(fmt.Sprintf("%d. 【%s】%s\n", n, label, strings.Join(glosses, "; ")))
	}
	b.WriteString("\n要求：\n" +
		"1. senses：按词条实际义项归纳，每个义项用 1-2 个简洁中文词/短语；不要编造词条没有的义项；" +
		"生僻、古语义项可省略；带领域标注的义项要保留领域（如【相扑】），不要按字面硬译。pos 用中文词性。\n" +
		"2. examples：必须给满 3 个自然、常用的日语例句（每句 ≤ 35 字），覆盖主要义项，并附准确中文翻译；" +
		"例句中要用到该词（可用活用形），必须是地道日语，不要混入英文或中文词（「公寓」应写「アパート」），也不要用具体人名、地名。\n" +
		"3. 不要输出解释、假名注音或 Markdown。\n" +
		"只输出 JSON：{\"senses\":[{\"pos\":\"名词\",\"zh\":[\"房间\",\"屋子\"]}]," +
		"\"examples\":[{\"jp\":\"部屋を掃除しました。\",\"zh\":\"打扫了房间。\"}]}")
	return b.String()
}

func entryTitle(item Item) string {
	surface := displaySurface(item)
	if surface == "" {
		return ""
	}
	if len(item.Readings) > 0 && item.Readings[0] != surface {
		return surface + "（" + item.Readings[0] + "）"
	}
	return surface
}

// posHint 把 JMdict 词性代码折成中文提示，降低模型猜词性代码的错误率。
func posHint(codes []string) string {
	out := []string{}
	for _, c := range codes {
		switch {
		case c == "n" || strings.HasPrefix(c, "n-"):
			out = appendUnique(out, "名词")
		case c == "v1" || strings.HasPrefix(c, "v1"):
			out = appendUnique(out, "一段动词")
		case strings.HasPrefix(c, "v5"):
			out = appendUnique(out, "五段动词")
		case c == "vs" || c == "vs-i":
			out = appendUnique(out, "サ变动词")
		case c == "vi":
			out = appendUnique(out, "自动词")
		case c == "vt":
			out = appendUnique(out, "他动词")
		case c == "adj-i" || c == "adj-ix":
			out = appendUnique(out, "形容词")
		case strings.HasPrefix(c, "adj-na"):
			out = appendUnique(out, "形容动词")
		case c == "adv" || c == "adv-to":
			out = appendUnique(out, "副词")
		case c == "exp":
			out = appendUnique(out, "惯用表达")
		case c == "prt":
			out = appendUnique(out, "助词")
		case c == "conj":
			out = appendUnique(out, "接续词")
		case c == "num":
			out = appendUnique(out, "数词")
		case c == "pn":
			out = appendUnique(out, "代词")
		case c == "ctr":
			out = appendUnique(out, "量词")
		case c == "pref":
			out = appendUnique(out, "前缀")
		case c == "suf":
			out = appendUnique(out, "后缀")
		default:
			out = appendUnique(out, c)
		}
	}
	if len(out) == 0 {
		return "未标注"
	}
	return strings.Join(out, "/")
}

func sanitizeZhSenses(src []zhSense) []zhSense {
	out := []zhSense{}
	for _, s := range src {
		pos := strings.TrimSpace(s.Pos)
		if len([]rune(pos)) > 12 {
			pos = ""
		}
		zh := []string{}
		seen := map[string]bool{}
		for _, z := range s.Zh {
			z = strings.TrimSpace(z)
			if z == "" || seen[z] || len([]rune(z)) > 30 || len(zh) >= 4 {
				continue
			}
			if !hasHan(z) || hasLatin(z) || strings.ContainsAny(z, "_^`") {
				continue
			}
			seen[z] = true
			zh = append(zh, z)
		}
		if len(zh) == 0 {
			continue
		}
		if pos == "" {
			pos = "释义"
		}
		out = append(out, zhSense{Pos: pos, Zh: zhList(zh)})
		if len(out) >= 5 {
			break
		}
	}
	return out
}

func sanitizeExamples(src []zhExample) []zhExample {
	out := []zhExample{}
	seen := map[string]bool{}
	for _, ex := range src {
		jp := strings.TrimSpace(ex.Jp)
		zh := strings.TrimSpace(ex.Zh)
		if jp == "" || zh == "" || seen[jp] || len([]rune(jp)) > 70 || len([]rune(zh)) > 70 {
			continue
		}
		if !hasJapanese(jp) || !hasKana(jp) || hasLatin(jp) || hasInvalidRune(jp) {
			continue
		}
		seen[jp] = true
		out = append(out, zhExample{Jp: jp, Zh: zh, Ruby: generate.BuildRubyTokens(jp)})
		if len(out) >= 3 {
			break
		}
	}
	return out
}

// readingFromExamples 从例句振假名里取该词的读音，保证读音与发音一致。
func readingFromExamples(examples []zhExample, q string) string {
	for _, ex := range examples {
		for _, tok := range ex.Ruby {
			if len(tok) < 2 {
				continue
			}
			s, _ := tok[0].(string)
			r, _ := tok[1].(string)
			if s == q && r != "" && isKana(r) {
				return r
			}
		}
	}
	return ""
}

// verifyExamples 让模型自检例句：剔除非日语、混入中文/英文、与词条无关或翻译不符的句子。
func (h *Handler) verifyExamples(ctx context.Context, userID string, item Item, examples []zhExample) []zhExample {
	if len(examples) == 0 {
		return examples
	}
	if os.Getenv("DICT_DEBUG") != "" {
		for i, ex := range examples {
			fmt.Printf("[dict] check %s #%d %s\n", entryTitle(item), i+1, ex.Jp)
		}
	}
	var b strings.Builder
	b.WriteString("下面是日语词条「" + entryTitle(item) + "」的例句（日语＋中文翻译）：\n")
	for i, ex := range examples {
		b.WriteString(fmt.Sprintf("%d. %s（%s）\n", i+1, ex.Jp, ex.Zh))
	}
	b.WriteString("请找出存在以下问题的例句序号：\n" +
		"①不是自然、地道的日语，或混入了中文/英文；\n" +
		"②与词条含义不符，或句子里根本没有用到该词（含活用形）；\n" +
		"③中文翻译与日语句子意思不符；\n" +
		"④出现中文或外国的人名、地名等专有名词。\n" +
		"只标出明显有问题的句子，不确定是否合格的一律视为合格。\n" +
		"只输出有问题的序号 JSON，如 {\"bad\":[2]}；全部合格输出 {\"bad\":[]}。不要解释。")
	start := time.Now()
	content, pt, ct, err := h.client.ChatJSONNoThinking(ctx, b.String())
	h.logLLM(ctx, userID, pt, ct, time.Since(start))
	if err != nil {
		return examples
	}
	var resp struct {
		Bad []int `json:"bad"`
	}
	raw := llmclient.ExtractJSON(content)
	if os.Getenv("DICT_DEBUG") != "" {
		fmt.Printf("[dict] verify %s bad=%s\n", entryTitle(item), raw)
	}
	if json.Unmarshal([]byte(raw), &resp) != nil {
		// 模型可能直接输出数组而不是 {"bad":[...]}
		if json.Unmarshal([]byte(raw), &resp.Bad) != nil {
			return examples
		}
	}
	out := []zhExample{}
	for i, ex := range examples {
		bad := false
		for _, n := range resp.Bad {
			if n == i+1 {
				bad = true
				break
			}
		}
		if !bad {
			out = append(out, ex)
		}
	}
	return out
}

// mergeExamples 追加例句并去重，最多保留 max 条。
func mergeExamples(base, more []zhExample, max int) []zhExample {
	out := append([]zhExample{}, base...)
	seen := map[string]bool{}
	for _, ex := range out {
		seen[ex.Jp] = true
	}
	for _, ex := range more {
		if seen[ex.Jp] {
			continue
		}
		seen[ex.Jp] = true
		out = append(out, ex)
		if len(out) >= max {
			break
		}
	}
	return out
}

// hasInvalidRune 只放行日文、汉字、数字与常用标点，拦截其他文字和占位符。
func hasInvalidRune(s string) bool {
	const punct = "、。，．！？!?…‥「」『』（）()【】・ー〜～：:；;,. "
	for _, r := range s {
		switch {
		case r >= 0x3041 && r <= 0x30FF: // 平假名/片假名
		case unicode.Is(unicode.Han, r):
		case r >= '0' && r <= '9':
		case r >= '０' && r <= '９':
		case strings.ContainsRune(punct, r):
		default:
			return true
		}
	}
	return false
}

// hasLatin 判断是否含拉丁字母：AI 例句偶尔混入英文碎片，直接丢弃。
func hasLatin(s string) bool {
	for _, r := range s {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') {
			return true
		}
	}
	return false
}

func sensesHash(raw []byte) string {
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:8])
}

// saveZh 写回词条中文缓存；search_text 供中文反查命中。
func (h *Handler) saveZh(ctx context.Context, entryID int64, hash string, c *zhContent) {
	words := []string{}
	for _, s := range c.Senses {
		words = append(words, s.Zh...)
	}
	payload, err := json.Marshal(c)
	if err != nil {
		return
	}
	_, _ = h.pool.Exec(ctx, `
		INSERT INTO dict_zh (entry_id, payload, search_text, model, src_hash)
		VALUES ($1, $2, $3, $4, $5)
		ON CONFLICT (entry_id) DO UPDATE
		  SET payload = EXCLUDED.payload, search_text = EXCLUDED.search_text,
		      model = EXCLUDED.model, src_hash = EXCLUDED.src_hash, updated_at = now()`,
		entryID, payload, strings.Join(words, " "), os.Getenv("LLM_MODEL"), hash)
}
