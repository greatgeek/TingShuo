// Package wordphrase 生词「常用词组」：每个词缓存 5 条短搭配（日文+假名+中文），
// 其中至少 2 条为动宾短语；由大模型按词生成，生词本词卡展开区展示并可点读。
package wordphrase

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"unicode"

	"github.com/greatgeek/TingShuo/backend/internal/generate"
	"github.com/greatgeek/TingShuo/backend/pkg/llmclient"
)

// Item 一条词组：text 日文 / reading 全假名 / ruby_tokens 逐块振假名 / translation 中文。
type Item struct {
	Text        string  `json:"text"`
	Reading     string  `json:"reading"`
	Ruby        [][]any `json:"ruby_tokens"`
	Translation string  `json:"translation"`
}

// WordInfo 生成词组所需的词信息；ExampleText 为可选参考例句。
type WordInfo struct {
	Surface     string
	Reading     string
	Pos         string
	Meaning     string
	ExampleText string
}

// Result 单个词的生成结果；Problems 记录被剔除的条目原因，便于批量跑批排查。
type Result struct {
	Items    []Item
	Problems []string
}

// GenStats 一次生成调用的 token 用量，供成本观测。
type GenStats struct {
	PromptTokens     int
	CompletionTokens int
}

// WantCount 每个词的目标词组条数；其中至少 2 条应为动宾短语。
const WantCount = 5

// MergeItems 合并已有条目与补写条目：按 Text 去重，保留已有在前，最多 WantCount 条。
func MergeItems(existing, add []Item) []Item {
	out := make([]Item, 0, len(existing)+len(add))
	seen := map[string]bool{}
	for _, it := range existing {
		if seen[it.Text] {
			continue
		}
		seen[it.Text] = true
		out = append(out, it)
	}
	for _, it := range add {
		if seen[it.Text] {
			continue
		}
		seen[it.Text] = true
		out = append(out, it)
	}
	if len(out) > WantCount {
		out = out[:WantCount]
	}
	return out
}

type llmPhrase struct {
	Text        string `json:"text"`
	Reading     string `json:"reading"`
	Translation string `json:"zh"`
}

type llmResult struct {
	Items []struct {
		ID      int         `json:"id"`
		Phrases []llmPhrase `json:"phrases"`
	} `json:"items"`
}

// Generate 为一批词各生成 5 条常用词组（至少 2 条动宾），结果按 words 下标返回。
func Generate(ctx context.Context, c *llmclient.Client, words []WordInfo, retry bool) (map[int]Result, GenStats, error) {
	out := map[int]Result{}
	if len(words) == 0 {
		return out, GenStats{}, nil
	}
	content, pt, ct, err := c.ChatJSON(ctx, buildPrompt(words, retry))
	if err != nil {
		return out, GenStats{}, err
	}
	var res llmResult
	if err := json.Unmarshal([]byte(llmclient.ExtractJSON(content)), &res); err != nil {
		return out, GenStats{PromptTokens: pt, CompletionTokens: ct},
			fmt.Errorf("解析模型 JSON 失败: %w", err)
	}
	for _, it := range res.Items {
		if it.ID < 1 || it.ID > len(words) {
			continue
		}
		idx := it.ID - 1
		items, problems := normalize(words[idx], it.Phrases)
		out[idx] = Result{Items: items, Problems: problems}
	}
	return out, GenStats{PromptTokens: pt, CompletionTokens: ct}, nil
}

// SupplementInput 为一个已有词组的词补写所需上下文：已有条目与还差几条。
type SupplementInput struct {
	Info     WordInfo
	Existing []Item
	Need     int
}

// Supplement 为一批已有词组的词按缺口补写（优先动宾），结果按 inputs 下标返回。
func Supplement(ctx context.Context, c *llmclient.Client, inputs []SupplementInput, retry bool) (map[int]Result, GenStats, error) {
	out := map[int]Result{}
	if len(inputs) == 0 {
		return out, GenStats{}, nil
	}
	content, pt, ct, err := c.ChatJSON(ctx, buildSupplementPrompt(inputs, retry))
	if err != nil {
		return out, GenStats{}, err
	}
	var res llmResult
	if err := json.Unmarshal([]byte(llmclient.ExtractJSON(content)), &res); err != nil {
		return out, GenStats{PromptTokens: pt, CompletionTokens: ct},
			fmt.Errorf("解析模型 JSON 失败: %w", err)
	}
	for _, it := range res.Items {
		if it.ID < 1 || it.ID > len(inputs) {
			continue
		}
		idx := it.ID - 1
		existing := make([]string, 0, len(inputs[idx].Existing))
		for _, p := range inputs[idx].Existing {
			existing = append(existing, p.Text)
		}
		items, problems := normalizeWith(inputs[idx].Info, it.Phrases, existing, inputs[idx].Need)
		out[idx] = Result{Items: items, Problems: problems}
	}
	return out, GenStats{PromptTokens: pt, CompletionTokens: ct}, nil
}

func buildSupplementPrompt(inputs []SupplementInput, retry bool) string {
	var b strings.Builder
	b.WriteString(`你是日语教材编者。下面每个单词已有若干“常用词组”，请按“还差”条数补写，使每个词共有 5 条。
要求：
1. 优先动宾短语：名词作宾语时用「〜を+动词」（如 本を読む、電話をかける）；单词本身是动词时，写出该动词+常见宾语（如 日本語を勉強する）。本次补写优先 2 条（还差不足 2 条时则全部）为动宾；动宾必须自然常用，宁缺毋滥。
2. 其余补写条目可覆盖不同搭配（名词+助词+动词、形容词+名词、固定说法），短小自然。
3. 每条是 2～10 个日文字符，必须使用该单词本身：动词/形容词可以活用，名词保持原形；带助词，不要整句、标点、解释。
4. 不得与“已有”条目重复，也不得只换助词。
5. reading 用平假名写出整条读音（外来语可用片假名），不要汉字、空格；zh 不超过 12 字。
6. 状态、性质名词（如 満員、元気）或寒暄、助词、形容词等无法自然构成动宾时，给最常见的自然搭配，不要硬凑。
只输出 JSON，不要 markdown，不要额外文字。格式：
{"items":[{"id":1,"phrases":[{"text":"東京を訪れる","reading":"とうきょうをおとずれる","zh":"访问东京"},{"text":"東京を離れる","reading":"とうきょうをはなれる","zh":"离开东京"}]}]}
单词列表：
`)
	for i, in := range inputs {
		fmt.Fprintf(&b, "%d. %s", i+1, in.Info.Surface)
		if in.Info.Reading != "" {
			fmt.Fprintf(&b, "（%s）", in.Info.Reading)
		}
		if in.Info.Pos != "" {
			fmt.Fprintf(&b, " [%s]", in.Info.Pos)
		}
		if in.Info.Meaning != "" {
			fmt.Fprintf(&b, " 释义：%s", in.Info.Meaning)
		}
		texts := make([]string, 0, len(in.Existing))
		for _, p := range in.Existing {
			texts = append(texts, p.Text)
		}
		fmt.Fprintf(&b, " 已有：%s 还差：%d\n", strings.Join(texts, "、"), in.Need)
	}
	if retry {
		b.WriteString("\n注意：上一次补写结果有不合格项。只输出 JSON；每条必须含该单词、长度 2～10 个日文字符、读音全为假名、中文简短，优先动宾且不要与已有重复。\n")
	}
	return b.String()
}

func buildPrompt(words []WordInfo, retry bool) string {
	var b strings.Builder
	b.WriteString(`你是日语教材编者。为下面每个单词写 5 条“常用固定搭配/词组”，帮助中国学习者把词记在语境里。
要求：
1. 优先 2 条动宾短语：名词作宾语时用「〜を+动词」（如 本を読む、電話をかける）；单词本身是动词时，写出该动词+常见宾语（如 日本語を勉強する）。动宾必须自然常用，宁缺毋滥。
2. 其余条目尽量覆盖不同搭配（名词+助词+动词、形容词+名词、固定说法），短小自然。
3. 每条是 2～10 个日文字符，必须使用该单词本身：动词/形容词可以活用，名词保持原形；带助词，不要整句，不要标点，不要解释。
4. reading 用平假名写出整条词组的读音（外来语可用片假名），不要汉字、空格。
5. zh 是简短中文释义（不超过 12 字）。
6. 状态、性质名词（如 満員、元気）或助词、感叹词等无法自然构成动宾时，给最常见的固定说法，不要硬凑。
只输出 JSON，不要 markdown，不要额外文字。格式：
{"items":[{"id":1,"phrases":[{"text":"東京を訪れる","reading":"とうきょうをおとずれる","zh":"访问东京"},{"text":"東京を離れる","reading":"とうきょうをはなれる","zh":"离开东京"},{"text":"東京駅","reading":"とうきょうえき","zh":"东京站"},{"text":"東京の街","reading":"とうきょうのまち","zh":"东京的街道"},{"text":"東京に行く","reading":"とうきょうにいく","zh":"去东京"}]}]}
单词列表：
`)
	for i, w := range words {
		fmt.Fprintf(&b, "%d. %s", i+1, w.Surface)
		if w.Reading != "" {
			fmt.Fprintf(&b, "（%s）", w.Reading)
		}
		if w.Pos != "" {
			fmt.Fprintf(&b, " [%s]", w.Pos)
		}
		if w.Meaning != "" {
			fmt.Fprintf(&b, " 释义：%s", w.Meaning)
		}
		if w.ExampleText != "" {
			fmt.Fprintf(&b, " 参考例句：%s", w.ExampleText)
		}
		b.WriteString("\n")
	}
	if retry {
		b.WriteString("\n注意：上一次这些词的结果有不合格项。每个词需 5 条（至少 2 条动宾）；每条必须包含该单词、长度 2～10 个日文字符、读音全为假名、中文简短，且不要重复。\n")
	}
	return b.String()
}

// normalize 校验模型输出：必须含词干、长度合理、读音为假名、中文非空、无重复。
// 振り仮名以词典为准，词条本体强制使用词库读音；模型读音只用于发现词典误读。
func normalize(w WordInfo, in []llmPhrase) ([]Item, []string) {
	return normalizeWith(w, in, nil, WantCount)
}

func normalizeWith(w WordInfo, in []llmPhrase, existing []string, target int) ([]Item, []string) {
	var out []Item
	var problems []string
	seen := map[string]bool{}
	for _, s := range existing {
		seen[strings.TrimSpace(s)] = true
	}
	maxLen := 12
	if n := len([]rune(w.Surface)); n+6 > maxLen {
		maxLen = n + 6
	}
	for _, p := range in {
		text := strings.TrimSpace(p.Text)
		reading := strings.TrimSpace(p.Reading)
		zh := strings.TrimSpace(p.Translation)
		switch {
		case text == "" || reading == "" || zh == "":
			problems = append(problems, fmt.Sprintf("字段缺失：%q", text))
			continue
		case strings.ContainsAny(text, "。！？!?，、,.；;：:"):
			problems = append(problems, fmt.Sprintf("含标点：%s", text))
			continue
		}
		n := len([]rune(text))
		if n < 2 || n > maxLen {
			problems = append(problems, fmt.Sprintf("长度 %d 不合规：%s", n, text))
			continue
		}
		if !isAllowedPhraseText(w.Surface, text) {
			problems = append(problems, fmt.Sprintf("含非日文字符：%s", text))
			continue
		}
		if strings.Contains(text, "辛苦了") || strings.Contains(text, "元左右") {
			problems = append(problems, fmt.Sprintf("中文短语：%s", text))
			continue
		}
		if !containsHeadword(w.Surface, text) {
			problems = append(problems, fmt.Sprintf("未包含词干 %s：%s", w.Surface, text))
			continue
		}
		if !generate.IsJapaneseWord(text) {
			problems = append(problems, fmt.Sprintf("非日语词形：%s", text))
			continue
		}
		if !isKanaReading(reading) {
			problems = append(problems, fmt.Sprintf("读音非假名：%s", reading))
			continue
		}
		// 名词词组读音必须包含词条读音，挡住「先生社会中」这类混入随机词元的脏结果
		if strings.Contains(w.Pos, "名") && isKanaReading(w.Reading) &&
			!strings.Contains(kataToHira(reading), kataToHira(w.Reading)) {
			problems = append(problems, fmt.Sprintf("读音不含词条读音：%s", reading))
			continue
		}
		if seen[text] {
			continue
		}
		seen[text] = true
		ruby, full, ok := phraseRuby(w, text, reading)
		if !ok {
			problems = append(problems, fmt.Sprintf("注音失败：%s", text))
			continue
		}
		out = append(out, Item{Text: text, Reading: full, Ruby: ruby, Translation: zh})
		if len(out) == target {
			break
		}
	}
	return out, problems
}

// phraseRuby 给词组生成逐块振り仮名与整条读音。词典分词后把词条本体（可能被切成
// 多个词元）合并并强制用词库读音；其余词元的读音若与模型读音对不上，视为词典
// 误读——该块只保留文字不显示假名，整条读音改用模型提供的假名。
// 数字/拉丁字母（5人いる、Eメール…）会被词典切碎且量词读音不可靠，这类词组不做
// 逐词注音，只保留模型给的整条假名。
func phraseRuby(w WordInfo, text, llmReading string) ([][]any, string, bool) {
	llmKana := kataToHira(llmReading)
	if hasDigitOrLatin(text) {
		if !isKanaReading(llmKana) {
			return nil, "", false
		}
		return [][]any{{text, nil}}, llmKana, true
	}
	toks := generate.BuildRubyTokens(text)
	if w.Surface != "" && isKanaReading(w.Reading) {
		toks = overrideHeadword(toks, w.Surface, w.Reading)
	}
	trust := true
	for _, tok := range toks {
		s, _ := tok[0].(string)
		// 词条本体有词库读音时已在 overrideHeadword 强制，跳过；没有读音时仍按
		// 词典读音与模型读音比对，避免「後→ご」这类词典误读直接显示出来。
		if s == w.Surface && isKanaReading(w.Reading) {
			continue
		}
		if !hasKanji(s) {
			continue
		}
		rd, ok := tok[1].(string)
		if !ok || rd == "" {
			continue
		}
		if !strings.Contains(llmKana, kataToHira(rd)) {
			trust = false
			tok[1] = nil
		}
	}
	var b strings.Builder
	for _, tok := range toks {
		s, _ := tok[0].(string)
		if rd, ok := tok[1].(string); ok && rd != "" {
			b.WriteString(rd)
		} else {
			b.WriteString(s)
		}
	}
	full := b.String()
	if !trust {
		full = llmKana
	}
	if !isKanaReading(full) {
		return nil, "", false
	}
	return toks, full, true
}

// overrideHeadword 在词元序列中定位词条本身（可能被词典切成多个词元），
// 合并为一个词块并改用词库读音，避免「預ける→あづける」这类词典误读。
func overrideHeadword(toks [][]any, surface, reading string) [][]any {
	out := make([][]any, 0, len(toks))
	for i := 0; i < len(toks); i++ {
		s, _ := toks[i][0].(string)
		if s != "" && strings.HasPrefix(surface, s) {
			acc := ""
			j := i
			for j < len(toks) && len(acc) < len(surface) {
				tj, _ := toks[j][0].(string)
				acc += tj
				j++
				if acc == surface || !strings.HasPrefix(surface, acc) {
					break
				}
			}
			if acc == surface {
				out = append(out, []any{surface, reading})
				i = j - 1
				continue
			}
		}
		out = append(out, toks[i])
	}
	return out
}

func hasKanji(s string) bool {
	for _, r := range s {
		if unicode.Is(unicode.Scripts["Han"], r) {
			return true
		}
	}
	return false
}

func kataToHira(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		if r >= 0x30A1 && r <= 0x30F6 {
			r -= 0x60
		}
		b.WriteRune(r)
	}
	return b.String()
}

// containsHeadword 判断词组是否用到了该词：优先整词匹配，其次匹配开头连续汉字词干
// （食べる→食、東京→東京、引き出し→引）；纯假名词要求完整出现。
// 词面含括号注释或空格时（サイズ(size)、A 型），去掉注释与空白后再比对。
func containsHeadword(surface, text string) bool {
	surface = sanitizeHeadword(strings.TrimSpace(strings.Trim(surface, "〜~")))
	if surface == "" {
		return true
	}
	if strings.Contains(sanitizeHeadword(text), surface) {
		return true
	}
	runes := []rune(surface)
	var stem []rune
	for _, r := range runes {
		if !unicode.Is(unicode.Scripts["Han"], r) {
			break
		}
		stem = append(stem, r)
	}
	return len(stem) > 0 && strings.Contains(sanitizeHeadword(text), string(stem))
}

func isKanaReading(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		switch {
		case r >= 0x3041 && r <= 0x309F: // 平假名
		case r >= 0x30A1 && r <= 0x30FF: // 片假名（外来语）
		default:
			return false
		}
	}
	return true
}

// isAllowedPhraseText 词形只允许汉字、假名、长音、中点、叠字符号：挡住混入拉丁字母的脏结果。
// 词面本身含数字/字母时（5、1時間、SNS、Eメール），对应字符也放行，否则「5人いる」
// 这类完全正确的搭配会被误杀。
func isAllowedPhraseText(surface, s string) bool {
	if s == "" {
		return false
	}
	allowDigit, allowLatin := false, false
	for _, r := range sanitizeHeadword(surface) {
		switch {
		case r >= '0' && r <= '9':
			allowDigit = true
		case r >= 'A' && r <= 'Z', r >= 'a' && r <= 'z':
			allowLatin = true
		}
	}
	for _, r := range s {
		switch {
		case strings.ContainsRune(chineseOnlyChars, r):
			return false
		case unicode.Is(unicode.Scripts["Han"], r):
		case r >= 0x3041 && r <= 0x309F:
		case r >= 0x30A1 && r <= 0x30FF:
		case r == 0x3005: // 々
		case r >= '0' && r <= '9', r >= '０' && r <= '９':
			if !allowDigit {
				return false
			}
		case r >= 'A' && r <= 'Z', r >= 'a' && r <= 'z', r >= 'Ａ' && r <= 'Ｚ', r >= 'ａ' && r <= 'ｚ':
			if !allowLatin {
				return false
			}
		default:
			return false
		}
	}
	return true
}

// sanitizeHeadword 去掉括号注释与空白、全角 ASCII 转半角，用于词面与词组的包含比对。
func sanitizeHeadword(s string) string {
	var b strings.Builder
	depth := 0
	for _, r := range s {
		switch r {
		case '(', '（':
			depth++
			continue
		case ')', '）':
			if depth > 0 {
				depth--
			}
			continue
		}
		if depth > 0 {
			continue
		}
		if r == ' ' || r == '　' {
			continue
		}
		if r >= 0xFF01 && r <= 0xFF5E {
			r -= 0xFEE0
		}
		b.WriteRune(r)
	}
	return b.String()
}

// hasDigitOrLatin 判断词组是否含数字/拉丁字母（全角按半角算）。
func hasDigitOrLatin(s string) bool {
	for _, r := range s {
		switch {
		case r >= '0' && r <= '9', r >= '０' && r <= '９':
			return true
		case r >= 'A' && r <= 'Z', r >= 'a' && r <= 'z', r >= 'Ａ' && r <= 'Ｚ', r >= 'ａ' && r <= 'ｚ':
			return true
		}
	}
	return false
}

// chineseOnlyChars 只在简体中文里出现、现代日语不用的字：模型偶尔混入中文短语
// （如「皆さん辛苦了」），这些条目读音/搭配都不成立，直接剔除。
const chineseOnlyChars = "们这说请谢让对错欢汉语读书听见钱买卖时间问题门东车马鸟鱼龙龟长关开为应该么办认识习练实现觉发亲爱儿岁处动员团围图场园圆坏块坚报择挂挤挥损换据显术杀杂权极构标样树检毕汤沟泽洁测济涛润涨渐滚滨滩"