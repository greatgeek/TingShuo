// Package dict 查词：JMdict 词条检索 + 中文反查。
// 日语查询走精确/前缀/辞书形还原；纯汉字查询无精确命中时转中文反查
// （LLM 给候选，逐个到 JMdict 校验，结果按查询缓存）。
package dict

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"

	ipa "github.com/ikawaha/kagome-dict/ipa"
	"github.com/ikawaha/kagome/v2/tokenizer"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/greatgeek/TingShuo/backend/internal/auth"
	"github.com/greatgeek/TingShuo/backend/internal/httpx"
	"github.com/greatgeek/TingShuo/backend/pkg/llmclient"
	"github.com/greatgeek/TingShuo/backend/pkg/storage"
	"github.com/greatgeek/TingShuo/backend/pkg/ttsclient"
	"github.com/greatgeek/TingShuo/backend/pkg/ulid"
)

var (
	dictLim   = httpx.NewLimiter(30, 15) // 每用户 30/分钟（仅中文反查未命中缓存时计）
	dictDaily = httpx.NewDailyQuota(500) // 每用户每天最多 500 次 LLM 反查（成本护栏）
)

type Handler struct {
	pool   *pgxpool.Pool
	client *llmclient.Client
	azure  *ttsclient.Client
	store  *storage.Client
}

func NewHandler(pool *pgxpool.Pool, azure *ttsclient.Client, store *storage.Client) *Handler {
	return &Handler{
		pool:  pool,
		azure: azure,
		store: store,
		client: llmclient.New(
			os.Getenv("LLM_BASE_URL"), os.Getenv("LLM_API_KEY"), os.Getenv("LLM_MODEL")),
	}
}

// Form 是词条的一个表记（kind: k=汉字表记 r=假名）。
type Form struct {
	Text   string   `json:"text"`
	Kind   string   `json:"kind"`
	Common bool     `json:"common"`
	Tags   []string `json:"tags"`
}

// Item 是搜索结果卡片：词形、读音、词性、英文义项与已缓存的中文义项。
type Item struct {
	EntryID  int64    `json:"entry_id"`
	Forms    []Form   `json:"forms"`
	Readings []string `json:"readings"`
	Common   bool     `json:"common"`
	Pos      []string `json:"pos"`
	Glosses  []string `json:"glosses"`
	Zh       []string `json:"zh,omitempty"`
	Match    string   `json:"match"` // exact | prefix | base | zh
}

// Search GET /dict/search?q=
func (h *Handler) Search(w http.ResponseWriter, r *http.Request) {
	q := normalizeQuery(r.URL.Query().Get("q"))
	if q == "" {
		httpx.Err(w, httpx.BadRequest("Q_REQUIRED", "请输入要查询的词"))
		return
	}
	if len([]rune(q)) > 30 {
		httpx.Err(w, httpx.BadRequest("Q_TOO_LONG", "查询词过长（最多 30 字）"))
		return
	}
	ctx := r.Context()
	forms := jpForms(q)

	exact, err := h.searchForms(ctx, forms, 8, "exact")
	if err != nil {
		httpx.Err(w, httpx.Internal(err.Error()))
		return
	}
	items := exact
	mode := "jp"

	// 单字/两字汉字查询在精确命中之外补前缀建议（部 → 部屋/部分...）
	if len(exact) > 0 {
		n := len([]rune(q))
		if n == 1 || (n == 2 && hasHan(q)) {
			prefix, err := h.searchPrefix(ctx, forms, 8)
			if err != nil {
				httpx.Err(w, httpx.Internal(err.Error()))
				return
			}
			items = appendExcluding(items, prefix, 12)
		}
	}

	if len(exact) == 0 {
		prefix, err := h.searchPrefix(ctx, forms, 12)
		if err != nil {
			httpx.Err(w, httpx.Internal(err.Error()))
			return
		}
		items = prefix
		// 精确/前缀都没有时，按辞书形再试一次（食べました → 食べる）
		if len(items) == 0 {
			if bases := baseForms(q); len(bases) > 0 {
				baseItems, err := h.searchForms(ctx, bases, 5, "base")
				if err != nil {
					httpx.Err(w, httpx.Internal(err.Error()))
					return
				}
				items = baseItems
			}
		}
	}

	// 纯汉字且日语侧没有精确命中：可能是中文反查（房间 → 部屋）
	if len(exact) == 0 && looksChinese(q) {
		zhItems, err := h.reverse(ctx, auth.FromContext(ctx), q, 10)
		if err == nil && len(zhItems) > 0 {
			items, mode = zhItems, "zh"
		}
	}
	if items == nil {
		items = []Item{}
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{
		"query": q, "mode": mode, "items": items,
	})
}

// searchForms 按完整词形查词条，返回按「常用优先、词形短优先」排序的结果。
func (h *Handler) searchForms(ctx context.Context, forms []string, limit int, match string) ([]Item, error) {
	rows, err := h.pool.Query(ctx, `
		WITH hits AS (
			SELECT f.entry_id, bool_or(f.common) AS hit_common, min(length(f.form)) AS minlen
			FROM dict_form f
			WHERE f.form = ANY($1)
			GROUP BY f.entry_id
			ORDER BY hit_common DESC, minlen, f.entry_id
			LIMIT $2
		)
		SELECT e.id, e.common, e.senses, z.payload, `+formsJSON+`
		FROM hits h
		JOIN dict_entry e ON e.id = h.entry_id
		LEFT JOIN dict_zh z ON z.entry_id = e.id
		ORDER BY h.hit_common DESC, h.minlen, e.id`, forms, limit)
	if err != nil {
		return nil, err
	}
	return scanItems(rows, match)
}

// searchPrefix 按前缀查词条（部 → 部屋/部分...），限制候选词形长度，避免宽前缀扫出长尾词。
// 平假名/片假名两种写法各查一次（こんぴゅ → コンピューター）。
func (h *Handler) searchPrefix(ctx context.Context, forms []string, limit int) ([]Item, error) {
	q1 := forms[0]
	q2 := q1
	if len(forms) > 1 {
		q2 = forms[1]
	}
	maxRunes := len([]rune(q1)) + 4
	rows, err := h.pool.Query(ctx, `
		WITH hits AS (
			SELECT f.entry_id, bool_or(f.common) AS hit_common, min(length(f.form)) AS minlen
			FROM dict_form f
			WHERE ((f.form ~>=~ $1 AND f.form ~<~ $2) OR (f.form ~>=~ $3 AND f.form ~<~ $4))
			  AND length(f.form) <= $5
			GROUP BY f.entry_id
			ORDER BY hit_common DESC, minlen, f.entry_id
			LIMIT $6
		)
		SELECT e.id, e.common, e.senses, z.payload, `+formsJSON+`
		FROM hits h
		JOIN dict_entry e ON e.id = h.entry_id
		LEFT JOIN dict_zh z ON z.entry_id = e.id
		ORDER BY h.hit_common DESC, h.minlen, e.id`,
		q1, q1+"\U0010FFFF", q2, q2+"\U0010FFFF", maxRunes, limit)
	if err != nil {
		return nil, err
	}
	return scanItems(rows, "prefix")
}

// formsJSON 聚合词条全部表记；读音与词形分开展示，按汉字表记/常用/短词优先排序。
const formsJSON = `COALESCE((
		SELECT json_agg(json_build_object(
			'text', f2.form, 'kind', f2.kind, 'common', f2.common, 'tags', f2.tags)
			ORDER BY CASE f2.kind WHEN 'k' THEN 0 ELSE 1 END, f2.common DESC, length(f2.form), f2.form)
		FROM dict_form f2 WHERE f2.entry_id = e.id), '[]'::json)`

type rawItem struct {
	id     int64
	common bool
	senses []byte
	zh     []byte
	forms  []byte
}

func scanItems(rows pgx.Rows, match string) ([]Item, error) {
	defer rows.Close()
	items := []Item{}
	for rows.Next() {
		var raw rawItem
		if err := rows.Scan(&raw.id, &raw.common, &raw.senses, &raw.zh, &raw.forms); err != nil {
			return nil, err
		}
		items = append(items, buildItem(raw, match))
	}
	return items, rows.Err()
}

func buildItem(raw rawItem, match string) Item {
	var forms []Form
	_ = json.Unmarshal(raw.forms, &forms)
	if forms == nil {
		forms = []Form{}
	}
	readings := []string{}
	seenReading := map[string]bool{}
	for _, f := range forms {
		if f.Kind == "r" && !seenReading[f.Text] {
			seenReading[f.Text] = true
			readings = append(readings, f.Text)
		}
	}
	pos, glosses := previewSenses(raw.senses)
	var zh []string
	if len(raw.zh) > 0 {
		zh = previewZh(raw.zh)
	}
	return Item{
		EntryID: raw.id, Forms: forms, Readings: readings, Common: raw.common,
		Pos: pos, Glosses: glosses, Zh: zh, Match: match,
	}
}

type jmSense struct {
	PartOfSpeech []string `json:"partOfSpeech"`
	Field        []string `json:"field"`
	Misc         []string `json:"misc"`
	Gloss        []struct {
		Text string `json:"text"`
	} `json:"gloss"`
}

// previewSenses 提取卡片用的词性与英文义项（最多 4 个词性、6 条释义）。
func previewSenses(raw []byte) ([]string, []string) {
	var senses []jmSense
	if json.Unmarshal(raw, &senses) != nil {
		return []string{}, []string{}
	}
	pos := []string{}
	seenPos := map[string]bool{}
	glosses := []string{}
	seenGloss := map[string]bool{}
	for _, s := range senses {
		for _, p := range s.PartOfSpeech {
			if !seenPos[p] && len(pos) < 4 {
				seenPos[p] = true
				pos = append(pos, p)
			}
		}
		for _, g := range s.Gloss {
			t := strings.TrimSpace(g.Text)
			if t == "" || seenGloss[t] || len(glosses) >= 6 {
				continue
			}
			seenGloss[t] = true
			glosses = append(glosses, t)
		}
	}
	return pos, glosses
}

type zhPayload struct {
	Senses []struct {
		Pos string   `json:"pos"`
		Zh  []string `json:"zh"`
	} `json:"senses"`
}

// previewZh 提取缓存的中文义项（最多 4 条）。
func previewZh(raw []byte) []string {
	var p zhPayload
	if json.Unmarshal(raw, &p) != nil {
		return nil
	}
	out := []string{}
	seen := map[string]bool{}
	for _, s := range p.Senses {
		for _, z := range s.Zh {
			z = strings.TrimSpace(z)
			if z == "" || seen[z] || len(out) >= 4 {
				continue
			}
			seen[z] = true
			out = append(out, z)
		}
	}
	return out
}

// loadItems 按给定 id 顺序装配结果（中文反查候选排序用）。
func (h *Handler) loadItems(ctx context.Context, ids []int64) (map[int64]Item, error) {
	out := map[int64]Item{}
	if len(ids) == 0 {
		return out, nil
	}
	rows, err := h.pool.Query(ctx, `
		SELECT e.id, e.common, e.senses, z.payload, `+formsJSON+`
		FROM dict_entry e
		LEFT JOIN dict_zh z ON z.entry_id = e.id
		WHERE e.id = ANY($1)`, ids)
	if err != nil {
		return nil, err
	}
	items, err := scanItems(rows, "zh")
	if err != nil {
		return nil, err
	}
	for _, it := range items {
		out[it.EntryID] = it
	}
	return out, nil
}

// reverse 中文反查：缓存优先；未命中时 LLM 给候选并逐个在 JMdict 校验，
// 同时叠加已预翻词条的中文义项命中。LLM 失败只回退本地命中，不阻断搜索。
func (h *Handler) reverse(ctx context.Context, userID, q string, limit int) ([]Item, error) {
	var cached []int64
	if err := h.pool.QueryRow(ctx,
		`SELECT entry_ids FROM dict_reverse WHERE q = $1`, q).Scan(&cached); err == nil {
		return h.itemsByIDs(ctx, cached, limit), nil
	}

	lexical := []int64{}
	rows, err := h.pool.Query(ctx, `
		SELECT z.entry_id FROM dict_zh z
		JOIN dict_entry e ON e.id = z.entry_id
		WHERE z.search_text ILIKE $1
		ORDER BY e.common DESC, z.entry_id
		LIMIT 20`, "%"+likeEscape(q)+"%")
	if err == nil {
		for rows.Next() {
			var id int64
			if rows.Scan(&id) == nil {
				lexical = append(lexical, id)
			}
		}
		rows.Close()
	}

	llmIDs := []int64{}
	if os.Getenv("LLM_API_KEY") != "" && os.Getenv("LLM_BASE_URL") != "" &&
		dictLim.Allow(userID) && dictDaily.Allow(userID) {
		llmIDs = h.llmReverse(ctx, userID, q, limit)
	}

	ids := mergeIDs(llmIDs, lexical)
	if len(ids) == 0 {
		return nil, nil
	}
	if len(llmIDs) > 0 {
		// 只缓存有 LLM 参与的结果；纯本地命中会随预翻进度变化，不冻结
		_, _ = h.pool.Exec(ctx, `
			INSERT INTO dict_reverse (q, entry_ids, model) VALUES ($1, $2, $3)
			ON CONFLICT (q) DO UPDATE
			  SET entry_ids = EXCLUDED.entry_ids, model = EXCLUDED.model, created_at = now()`,
			q, ids, os.Getenv("LLM_MODEL"))
	}
	return h.itemsByIDs(ctx, ids, limit), nil
}

// llmReverse 让模型给出中文词对应的日语候选，并到词典校验词形/读音是否真实存在。
func (h *Handler) llmReverse(ctx context.Context, userID, q string, limit int) []int64 {
	prompt := "用户输入中文「" + q + "」，想查对应的日语单词（日中词典反查）。\n" +
		"请给出最多 " + strconv.Itoa(limit) + " 个最常用、最贴切的对应日语词，按常用度从高到低排序。\n" +
		"要求：\n" +
		"1. 只给真实存在的日语词形，不要生造词，不要重复。\n" +
		"2. surface 用日语常用表记（有汉字用汉字，外来语用片假名）；reading 用平假名（外来语可用片假名）。\n" +
		"3. 不要输出中文词或英文词，不要解释。\n" +
		"只输出 JSON：{\"items\":[{\"surface\":\"部屋\",\"reading\":\"へや\"}]}"
	start := time.Now()
	content, pt, ct, err := h.client.ChatJSONNoThinking(ctx, prompt)
	if err != nil {
		return nil
	}
	_, _ = h.pool.Exec(ctx, `
		INSERT INTO llm_call_log (id, user_id, lesson_id, model, prompt_tokens, output_tokens, duration_ms)
		VALUES ($1, $2, NULL, $3, $4, $5, $6)`,
		ulid.MustNew(), userID, os.Getenv("LLM_MODEL"), pt, ct, time.Since(start).Milliseconds())

	var resp struct {
		Items []struct {
			Surface string `json:"surface"`
			Reading string `json:"reading"`
		} `json:"items"`
	}
	if json.Unmarshal([]byte(llmclient.ExtractJSON(content)), &resp) != nil {
		return nil
	}

	type cand struct{ surface, reading string }
	cands := []cand{}
	seenCand := map[string]bool{}
	allForms := []string{}
	for _, it := range resp.Items {
		s := strings.TrimSpace(it.Surface)
		rd := strings.TrimSpace(it.Reading)
		if s == "" || len([]rune(s)) > 20 || !hasJapanese(s) || seenCand[s] {
			continue
		}
		if rd != "" && !isKana(rd) {
			continue
		}
		seenCand[s] = true
		cands = append(cands, cand{surface: s, reading: rd})
		allForms = append(allForms, s)
		if rd != "" && rd != s {
			allForms = append(allForms, rd)
		}
	}
	if len(cands) == 0 {
		return nil
	}

	byForm := map[string][]int64{}
	rows, err := h.pool.Query(ctx, `
		SELECT form, entry_id FROM dict_form WHERE form = ANY($1)`, allForms)
	if err != nil {
		return nil
	}
	for rows.Next() {
		var form string
		var id int64
		if rows.Scan(&form, &id) == nil {
			byForm[form] = append(byForm[form], id)
		}
	}
	rows.Close()

	ids := []int64{}
	seenID := map[int64]bool{}
	addID := func(id int64) {
		if !seenID[id] {
			seenID[id] = true
			ids = append(ids, id)
		}
	}
	for _, c := range cands {
		surfIDs := byForm[c.surface]
		// 词形与读音同时对上时优先取交集，避免同形异读取错词条
		if c.reading != "" && c.reading != c.surface {
			if readIDs := byForm[c.reading]; len(readIDs) > 0 && len(surfIDs) > 0 {
				readSet := map[int64]bool{}
				for _, id := range readIDs {
					readSet[id] = true
				}
				matched := int64(0)
				for _, id := range surfIDs {
					if readSet[id] {
						matched = id
						break
					}
				}
				if matched != 0 {
					addID(matched)
					continue
				}
			}
		}
		if len(surfIDs) > 0 {
			addID(surfIDs[0])
			continue
		}
		for _, id := range byForm[c.reading] {
			addID(id)
		}
	}
	return h.verifyReverse(ctx, userID, q, ids)
}

// verifyReverse 二道校验：带候选的词典英文释义回问模型，剔除释义不符的候选；
// 查询本身不是可查的词（如「没有这个词」）时会把候选全部剔除，避免硬凑结果。
func (h *Handler) verifyReverse(ctx context.Context, userID, q string, ids []int64) []int64 {
	byID, err := h.loadItems(ctx, ids)
	if err != nil || len(byID) == 0 {
		return ids
	}
	lines := []string{}
	for _, id := range ids {
		it, ok := byID[id]
		if !ok {
			continue
		}
		lines = append(lines, fmt.Sprintf("%d. %s（%s）%s",
			len(lines), displaySurface(it), strings.Join(it.Readings, "/"), strings.Join(it.Glosses, "; ")))
	}
	if len(lines) == 0 {
		return ids
	}
	prompt := "用户在学日语，输入了中文「" + q + "」，想查对应的日语词。下面是词典候选（括号内为读音，后面是词典英文释义）：\n" +
		strings.Join(lines, "\n") +
		"\n请逐个判断：该日语词是否确实是「" + q + "」的常用日语对应词。\n" +
		"注意：①释义与查询明显不符的标出；②只是字面或读音相近、意思不对的标出；" +
		"③如果查询本身不是可查的词（句子、口语短语、无意义内容），而候选都不对应，全部标出。\n" +
		"只输出存在问题的序号 JSON 数组（如 [1,3]）；全部合格输出 []。不要解释。"
	start := time.Now()
	raw, pt, ct, err := h.client.ChatJSONNoThinking(ctx, prompt)
	if err != nil {
		return ids
	}
	_, _ = h.pool.Exec(ctx, `
		INSERT INTO llm_call_log (id, user_id, lesson_id, model, prompt_tokens, output_tokens, duration_ms)
		VALUES ($1, $2, NULL, $3, $4, $5, $6)`,
		ulid.MustNew(), userID, os.Getenv("LLM_MODEL"), pt, ct, time.Since(start).Milliseconds())

	bad := map[int]bool{}
	for _, n := range parseIntArray(raw) {
		bad[n] = true
	}
	out := []int64{}
	for i, id := range ids {
		if !bad[i] {
			out = append(out, id)
		}
	}
	return out
}

func displaySurface(it Item) string {
	for _, f := range it.Forms {
		if f.Kind == "k" {
			return f.Text
		}
	}
	if len(it.Forms) > 0 {
		return it.Forms[0].Text
	}
	return ""
}

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

// likeEscape 转义 LIKE 通配符，查询里的 % _ 按字面匹配。
func likeEscape(s string) string {
	return strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`).Replace(s)
}

func (h *Handler) itemsByIDs(ctx context.Context, ids []int64, limit int) []Item {
	if len(ids) > limit {
		ids = ids[:limit]
	}
	byID, err := h.loadItems(ctx, ids)
	if err != nil {
		return nil
	}
	out := make([]Item, 0, len(ids))
	for _, id := range ids {
		if it, ok := byID[id]; ok {
			out = append(out, it)
		}
	}
	return out
}

func appendExcluding(dst, src []Item, limit int) []Item {
	seen := map[int64]bool{}
	for _, it := range dst {
		seen[it.EntryID] = true
	}
	for _, it := range src {
		if seen[it.EntryID] || len(dst) >= limit {
			continue
		}
		seen[it.EntryID] = true
		dst = append(dst, it)
	}
	return dst
}

func mergeIDs(groups ...[]int64) []int64 {
	seen := map[int64]bool{}
	out := []int64{}
	for _, g := range groups {
		for _, id := range g {
			if !seen[id] {
				seen[id] = true
				out = append(out, id)
			}
		}
	}
	return out
}

// ---- 文本归一化与辞书形还原 ----

// normalizeQuery 去首尾空白、全角 ASCII 折半角、全角空格折半角空格。
func normalizeQuery(s string) string {
	s = strings.TrimSpace(s)
	var b strings.Builder
	for _, r := range s {
		switch {
		case r == '\u3000':
			b.WriteRune(' ')
		case r >= 0xFF01 && r <= 0xFF5E:
			b.WriteRune(r - 0xFEE0)
		default:
			b.WriteRune(r)
		}
	}
	return strings.TrimSpace(b.String())
}

// jpForms 生成日语查询变体：纯假名查询同时按平假名/片假名各查一次。
func jpForms(q string) []string {
	out := []string{q}
	if isKana(q) {
		if hasKatakana(q) {
			out = appendUnique(out, kataToHira(q))
		}
		if hasHiragana(q) {
			out = appendUnique(out, hiraToKata(q))
		}
	}
	return out
}

func appendUnique(list []string, s string) []string {
	for _, x := range list {
		if x == s {
			return list
		}
	}
	return append(list, s)
}

func looksChinese(q string) bool {
	return hasHan(q) && !hasKana(q)
}

func hasJapanese(s string) bool {
	return hasHan(s) || hasKana(s)
}

func hasHan(s string) bool {
	for _, r := range s {
		if unicode.Is(unicode.Han, r) {
			return true
		}
	}
	return false
}

func hasHiragana(s string) bool {
	for _, r := range s {
		if r >= 0x3041 && r <= 0x3096 {
			return true
		}
	}
	return false
}

func hasKatakana(s string) bool {
	for _, r := range s {
		if r >= 0x30A1 && r <= 0x30FA {
			return true
		}
	}
	return false
}

func hasKana(s string) bool {
	for _, r := range s {
		if (r >= 0x3041 && r <= 0x3096) || (r >= 0x30A1 && r <= 0x30FA) ||
			r == 0x30FC || r == 0x30FB {
			return true
		}
	}
	return false
}

// isKana 判断整串是否只含假名（含长音/中点/叠字）。
func isKana(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if !(r >= 0x3041 && r <= 0x3096) && !(r >= 0x309D && r <= 0x309F) &&
			!(r >= 0x30A1 && r <= 0x30FA) && !(r >= 0x30FD && r <= 0x30FF) &&
			r != 0x30FC && r != 0x30FB {
			return false
		}
	}
	return true
}

func kataToHira(s string) string {
	rs := []rune(s)
	for i, r := range rs {
		if r >= 0x30A1 && r <= 0x30F6 {
			rs[i] = r - 0x60
		}
	}
	return string(rs)
}

func hiraToKata(s string) string {
	rs := []rune(s)
	for i, r := range rs {
		if r >= 0x3041 && r <= 0x3096 {
			rs[i] = r + 0x60
		}
	}
	return string(rs)
}

var (
	tokOnce sync.Once
	dictTok *tokenizer.Tokenizer
)

// baseForms 用 kagome 还原辞书形：食べました → 食べる、行った → 行く。
func baseForms(q string) []string {
	tokOnce.Do(func() {
		t, err := tokenizer.New(ipa.Dict())
		if err == nil {
			dictTok = t
		}
	})
	if dictTok == nil {
		return nil
	}
	out := []string{}
	for _, t := range dictTok.Tokenize(q) {
		if t.ID == tokenizer.BosEosID || t.Surface == "" {
			continue
		}
		// 只还原实词（动词/形容词）；助动词 ます/た 的辞书形是独立词，会把查询带偏
		pos := t.POS()
		if len(pos) == 0 || (pos[0] != "動詞" && pos[0] != "形容詞") {
			continue
		}
		if bf, ok := t.BaseForm(); ok && bf != "" && bf != t.Surface {
			out = appendUnique(out, bf)
			if len(out) >= 2 {
				break
			}
		}
	}
	return out
}
