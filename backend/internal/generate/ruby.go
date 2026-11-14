// 振り仮名增强（FR-2.12）：kagome 分词 + 词典读音 → ruby_tokens；
// LLM 整句假名仅作交叉校验参考，不一致记 warning，不阻塞落库。
package generate

import (
	"encoding/json"
	"os"
	"strings"
	"unicode"

	ipa "github.com/ikawaha/kagome-dict/ipa"
	"github.com/ikawaha/kagome/v2/tokenizer"
)

// ipaDict 全局加载一次（IPA 词典约 50MB，进程内共享，只读安全）。
var ipaDict = ipa.Dict()

// rubyOverride 逐词读音覆盖表（qa_ruby 产出的零星误读，免改代码修正），
// 优先级高于 kagome 与所有纠正表。文件缺失静默忽略。
var rubyOverride = loadRubyOverride()

func loadRubyOverride() map[string]string {
	cands := []string{
		os.Getenv("RUBY_OVERRIDE"),
		"tools/liangshuang/ruby_override.json",
		"../tools/liangshuang/ruby_override.json",
		"/opt/tingshuo/src/tools/liangshuang/ruby_override.json",
	}
	for _, p := range cands {
		if p == "" {
			continue
		}
		b, err := os.ReadFile(p)
		if err != nil {
			continue
		}
		var m map[string]string
		if json.Unmarshal(b, &m) == nil {
			return m
		}
	}
	return map[string]string{}
}

// readingFix 上下文读音纠正：kagome 按词元取词典读音，个别组合会误读
// （「この後」被拆成 この＋後(のち)，实读 このあと）。键为 [前一词元, 当前词元]。
var readingFix = map[[2]string]string{
	{"この", "後"}: "あと",
	{"の", "後"}:  "あと",
	{"の", "方"}:  "かた",
	{"この", "方"}: "かた",
	{"その", "方"}: "かた",
	{"あの", "方"}: "かた",
	{"の", "時"}:  "とき",
	{"この", "時"}: "とき",
	{"その", "時"}: "とき",
	{"あの", "時"}: "とき",
	{"何", "分"}:  "ぷん",
}

// singleFix 孤立词元读音纠正（无上下文歧义或语料中义项单一）。
var singleFix = map[string]string{
	"今":  "いま",
	"他人": "たにん",
	"形":  "かたち",
}

// nextFix 由后一词元决定的读音（ okurigana 被拆出时）：[当前词元, 后一词元]。
var nextFix = map[[2]string]string{
	{"失", "くし"}: "な",
	{"長", "い"}:  "なが",
	{"何", "月"}:  "なん",
}

// BuildRubyTokens 对句子分词并输出振り仮名结构：
// [["会計","かいけい"],["を",null],...]——含汉字词带平假名读音，其余 null。
// 词界按“学习词块”合并：词尾并入动词、名词与接尾合并、助词保持独立，
// 避免「重｜さ」「あり｜ます」这类过碎切分。
func BuildRubyTokens(text string) [][]any {
	t, err := tokenizer.New(ipaDict)
	if err != nil {
		// 词典加载失败时退化为纯文本 token（振り仮名缺失但不阻塞落库）
		return [][]any{{text, nil}}
	}
	toks := make([]rubyTok, 0, 16)
	for _, m := range t.Tokenize(text) {
		if m.ID == tokenizer.BosEosID {
			continue // BOS/EOS 占位词元（Surface 为字面量 "BOS"/"EOS"）
		}
		surface := m.Surface
		if surface == "" {
			continue
		}
		reading, _ := m.Reading()
		if reading != "" && containsKanji(surface) {
			toks = append(toks, rubyTok{surface: surface, reading: kataToHira(reading), pos: m.POS()})
		} else {
			toks = append(toks, rubyTok{surface: surface, pos: m.POS()})
		}
	}
	for i := 0; i < len(toks); i++ {
		if r, ok := rubyOverride[toks[i].surface]; ok && toks[i].reading != "" {
			toks[i].reading = r
			continue
		}
		if i > 0 {
			if r, ok := readingFix[[2]string{toks[i-1].surface, toks[i].surface}]; ok {
				toks[i].reading = r
				continue
			}
		}
		if i+1 < len(toks) {
			if r, ok := nextFix[[2]string{toks[i].surface, toks[i+1].surface}]; ok {
				toks[i].reading = r
				continue
			}
		}
		// 「時」：数词/何之后读 じ（三時・何時），其余读 とき（食べた時・小さい時）
		if toks[i].surface == "時" && toks[i].reading != "" {
			if i == 0 || !(isRubyNumeral(toks[i-1]) || toks[i-1].surface == "何") {
				toks[i].reading = "とき"
				continue
			}
		}
		if r, ok := singleFix[toks[i].surface]; ok && toks[i].reading != "" {
			toks[i].reading = r
		}
	}
	// 数词/何 + 月：日期读 がつ（4月、何月）；单独的「月」仍读 つき
	for i := 1; i < len(toks); i++ {
		if toks[i].surface == "月" && toks[i].reading != "" &&
			(isRubyNumeral(toks[i-1]) || toks[i-1].surface == "何") {
			toks[i].reading = "がつ"
		}
	}
	toks = mergeRubyTokens(toks)
	out := make([][]any, 0, len(toks))
	for _, tk := range toks {
		if tk.reading != "" {
			out = append(out, []any{tk.surface, tk.reading})
		} else {
			out = append(out, []any{tk.surface, nil})
		}
	}
	if len(out) == 0 {
		return [][]any{{text, nil}}
	}
	return out
}

// rubyTok 是合并阶段的词元：pos 始终保存最右成分的词性，
// 合并后继续按“上一个词块的词尾”判断能不能接ます/た等。
// nounChunk 标记已经合成过的名词词块（誕生日、重さ），允许继续吸收相邻名词。
type rubyTok struct {
	surface   string
	reading   string
	pos       []string
	nounChunk bool
}

// rubyCompounds 是 kagome 会切碎、但应作为一个学习词块记忆的固定说法。
var rubyCompounds = []string{
	"どれくらい", "なんとか", "ええと", "初めまして", "どういたしまして",
	"Wi-Fi", "年前",
}

// rubyPrefixJoin 只合并这些接頭詞（「今」也是接頭詞，但不能和后面的词粘在一起）。
var rubyPrefixJoin = map[string]bool{"お": true, "ご": true, "第": true}

func rubyPOS(pos []string, i int) string {
	if i < len(pos) {
		return pos[i]
	}
	return ""
}

func isRubyPunct(s string) bool {
	switch s {
	case "、", "。", "，", "．", "！", "？", "!", "?", "「", "」", "『", "』",
		"（", "）", "(", ")", "【", "】", "〈", "〉", "《", "》",
		"…", "‥", ":", "：", ";", "；", "~", "〜", "-", "‐", "—", "–", "・":
		return true
	}
	return false
}

const rubyNumeralRunes = "〇零一二三四五六七八九十百千万億兆"

// rubyCounters 用于识别词典已合成的数词（一つ、三枚）以及判断“数词+量词”不并入名词。
var rubyCounters = map[rune]bool{
	'つ': true, '人': true, '名': true, '枚': true, '個': true, '本': true, '杯': true,
	'匹': true, '台': true, '冊': true, '歳': true, '才': true, '階': true, '円': true,
	'年': true, '月': true, '日': true, '時': true, '分': true, '秒': true, '回': true,
	'度': true, '番': true, '号': true, '件': true, '曲': true, '話': true, '軒': true,
	'泊': true, '着': true, '足': true, '頭': true, '羽': true, '組': true,
}

func isRubyNumeral(t rubyTok) bool {
	if rubyPOS(t.pos, 0) == "名詞" && rubyPOS(t.pos, 1) == "数" {
		return true
	}
	// 一時/一緒 等含量词字的副词性名词不是数词
	if rubyPOS(t.pos, 0) == "名詞" && rubyPOS(t.pos, 1) == "副詞可能" {
		return false
	}
	// 一つ/三枚/一人 被词典合成一个词时词性不是“数”，按字面识别
	runes := []rune(t.surface)
	if len(runes) == 0 || !strings.ContainsRune(rubyNumeralRunes, runes[0]) {
		return false
	}
	if len(runes) == 1 {
		return true
	}
	restNumeral := true
	for _, r := range runes[1:] {
		if !strings.ContainsRune(rubyNumeralRunes, r) {
			restNumeral = false
			break
		}
	}
	if restNumeral {
		return true
	}
	return len(runes) == 2 && rubyCounters[runes[1]]
}

// isRubyNounBase 判断词元能否作为名词词块成分；副詞可能（今夜/今日）留给调用方
// 决定：作左项时不并入，作右项时可被前面的名词吸附（部屋全体）。
func isRubyNounBase(t rubyTok) bool {
	if rubyPOS(t.pos, 0) != "名詞" || isRubyNumeral(t) {
		return false
	}
	switch rubyPOS(t.pos, 1) {
	case "数", "代名詞", "非自立", "接尾":
		return false
	}
	return true
}

func isRubyNounMergeable(t rubyTok) bool {
	if t.nounChunk {
		return !isRubyNumeral(t)
	}
	return isRubyNounBase(t) && rubyPOS(t.pos, 1) != "副詞可能"
}

// matchRubyCompound 返回复合词结束下标（左闭右开）。
func matchRubyCompound(toks []rubyTok, i int) (int, bool) {
	for _, c := range rubyCompounds {
		acc := ""
		j := i
		for j < len(toks) && len(acc) < len(c) {
			acc += toks[j].surface
			j++
			if acc == c {
				return j, true
			}
		}
	}
	return 0, false
}

func combineRubyTokens(parts ...rubyTok) rubyTok {
	var surface, reading strings.Builder
	readingOK := true
	for _, p := range parts {
		surface.WriteString(p.surface)
		switch {
		case p.reading != "":
			reading.WriteString(p.reading)
		case containsKanji(p.surface):
			readingOK = false
		default:
			reading.WriteString(p.surface)
		}
	}
	out := rubyTok{surface: surface.String(), pos: parts[len(parts)-1].pos}
	// 各成分读音已统一为平假名；无读音的假名成分原样保留（アメリカ人 → アメリカじん）
	if readingOK && containsKanji(out.surface) {
		out.reading = reading.String()
	}
	return out
}

func mergeRubyTokens(toks []rubyTok) []rubyTok {
	out := make([]rubyTok, 0, len(toks))
	for i := 0; i < len(toks); {
		cur := toks[i]
		var prev *rubyTok
		if n := len(out); n > 0 {
			prev = &out[n-1]
		}
		prevOK := prev != nil && !isRubyPunct(prev.surface)

		// 数词＋つ：「1つと」会被 kagome 误切成「1｜つと」
		if prevOK && isRubyNumeral(*prev) && (cur.surface == "つ" || cur.surface == "つと") {
			out[len(out)-1] = combineRubyTokens(*prev, rubyTok{surface: "つ", pos: []string{"名詞", "接尾", "助数詞"}})
			if cur.surface == "つと" {
				out = append(out, rubyTok{surface: "と", pos: []string{"助詞", "格助詞"}})
			}
			i++
			continue
		}

		// 固定复合词（长匹配优先）
		if j, ok := matchRubyCompound(toks, i); ok {
			out = append(out, combineRubyTokens(toks[i:j]...))
			i = j
			continue
		}

		// お/ご/第 + 名词
		if rubyPOS(cur.pos, 0) == "接頭詞" && rubyPrefixJoin[cur.surface] &&
			i+1 < len(toks) && rubyPOS(toks[i+1].pos, 0) == "名詞" && !isRubyPunct(toks[i+1].surface) {
			m := combineRubyTokens(cur, toks[i+1])
			m.nounChunk = true
			out = append(out, m)
			i += 2
			continue
		}

		// 名词 + 接尾（中村さん、映画館、誕生日、会社員；数词后的量词不并）
		if prevOK && rubyPOS(cur.pos, 0) == "名詞" && rubyPOS(cur.pos, 1) == "接尾" &&
			rubyPOS(prev.pos, 0) == "名詞" && !isRubyNumeral(*prev) {
			m := combineRubyTokens(*prev, cur)
			m.nounChunk = true
			out[len(out)-1] = m
			i++
			continue
		}

		// 形容词词干 + さ（重さ、長さ、高さ：kagome 把「重」标成形容詞,自立）
		if prevOK && rubyPOS(cur.pos, 0) == "名詞" && rubyPOS(cur.pos, 1) == "接尾" &&
			rubyPOS(cur.pos, 2) == "特殊" && rubyPOS(prev.pos, 0) == "形容詞" {
			m := combineRubyTokens(*prev, cur)
			m.nounChunk = true
			out[len(out)-1] = m
			i++
			continue
		}

		// 助動詞：动词/形容词/助动词词尾并入前项；です/でし 保持独立
		if prevOK && rubyPOS(cur.pos, 0) == "助動詞" {
			attach := false
			switch cur.surface {
			case "です", "でし", "でしょ":
				attach = false
			case "ござい":
				attach = rubyPOS(prev.pos, 0) == "感動詞"
			case "な":
				attach = rubyPOS(prev.pos, 1) == "形容動詞語幹"
			case "ん":
				attach = rubyPOS(prev.pos, 0) == "動詞" || rubyPOS(prev.pos, 0) == "形容詞" ||
					rubyPOS(prev.pos, 0) == "助動詞" || rubyPOS(prev.pos, 1) == "形容動詞語幹"
			default:
				attach = rubyPOS(prev.pos, 0) == "動詞" || rubyPOS(prev.pos, 0) == "形容詞" ||
					rubyPOS(prev.pos, 0) == "助動詞"
			}
			if attach {
				out[len(out)-1] = combineRubyTokens(*prev, cur)
				i++
				continue
			}
		}

		// サ変名词 + し/する（お願いします、紹介します）
		if prevOK && rubyPOS(prev.pos, 1) == "サ変接続" && rubyPOS(cur.pos, 0) == "動詞" {
			switch cur.surface {
			case "し", "する", "さ", "せ", "しろ":
				out[len(out)-1] = combineRubyTokens(*prev, cur)
				i++
				continue
			}
		}

		// 非自立动词并入前项（休んでください、言ってくれました）
		if prevOK && rubyPOS(cur.pos, 0) == "動詞" && rubyPOS(cur.pos, 1) == "非自立" &&
			(rubyPOS(prev.pos, 0) == "動詞" || rubyPOS(prev.pos, 0) == "形容詞" ||
				rubyPOS(prev.pos, 1) == "サ変接続") {
			out[len(out)-1] = combineRubyTokens(*prev, cur)
			i++
			continue
		}

		// 接续助词 て/で/ば 并入前项（美味しくて、休んで、なければ）
		if prevOK && rubyPOS(cur.pos, 0) == "助詞" && rubyPOS(cur.pos, 1) == "接続助詞" &&
			(cur.surface == "て" || cur.surface == "で" || cur.surface == "ば") &&
			(rubyPOS(prev.pos, 0) == "動詞" || rubyPOS(prev.pos, 0) == "形容詞" ||
				rubyPOS(prev.pos, 0) == "助動詞") {
			out[len(out)-1] = combineRubyTokens(*prev, cur)
			i++
			continue
		}

		// 相邻名词合为一个词块（フードコート、留学｜生活、部屋全体）
		if prevOK && isRubyNounMergeable(*prev) && isRubyNounBase(cur) {
			m := combineRubyTokens(*prev, cur)
			m.nounChunk = true
			out[len(out)-1] = m
			i++
			continue
		}

		out = append(out, cur)
		i++
	}
	return out
}

// Reading 返回整句假名读音：有读音用读音，无读音的词元（假名/标点）用字面；
// 听写判分用它把用户的汉字写法折回读音，再与课文读音比对。
func Reading(text string) string {
	var b strings.Builder
	for _, tok := range BuildRubyTokens(text) {
		if len(tok) < 2 {
			continue
		}
		if rd, ok := tok[1].(string); ok && rd != "" {
			b.WriteString(rd)
			continue
		}
		if s, ok := tok[0].(string); ok {
			b.WriteString(s)
		}
	}
	return b.String()
}

// KanaCrossCheck 交叉校验（FR-2.11 简化版）：kagome 词典读音拼接 vs LLM 整句假名。
// 两串规范化（片→平、剥离数字/标点/空白）后按 LCS 比例相似，返回 [0,1]。
// M1 仅产出 warning；<0.5 视为明显异常。
func KanaCrossCheck(ruby [][]any, llmKana string) float64 {
	var kagomeKana strings.Builder
	for _, tok := range ruby {
		if len(tok) != 2 {
			continue
		}
		// 有读音用读音；无读音的 token（假名/标点）用自身
		if s, ok := tok[1].(string); ok && s != "" {
			kagomeKana.WriteString(s)
		} else if s, ok := tok[0].(string); ok {
			kagomeKana.WriteString(s)
		}
	}
	return lcsRatio(normalizeKana(kagomeKana.String()), normalizeKana(llmKana))
}

// normalizeKana：片假名→平假名、剥离空白、ASCII 数字与标点。
func normalizeKana(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case r < 0x80: // ASCII（数字/半角符号）剥离
			continue
		case unicode.Is(unicode.Space, r), unicode.IsPunct(r):
			continue
		case r >= 0x30A1 && r <= 0x30F6:
			r -= 0x60 // 片→平
		}
		b.WriteRune(r)
	}
	return b.String()
}

// lcsRatio 最长公共子序列长度 / 较长串长度（句子短，O(n²) DP 无压力）。
func lcsRatio(a, b string) float64 {
	ra, rb := []rune(a), []rune(b)
	if len(ra) == 0 || len(rb) == 0 {
		return 0
	}
	prev := make([]int, len(rb)+1)
	cur := make([]int, len(rb)+1)
	for i := 1; i <= len(ra); i++ {
		for j := 1; j <= len(rb); j++ {
			if ra[i-1] == rb[j-1] {
				cur[j] = prev[j-1] + 1
			} else if prev[j] >= cur[j-1] {
				cur[j] = prev[j]
			} else {
				cur[j] = cur[j-1]
			}
		}
		prev, cur = cur, prev
	}
	longest := len(ra)
	if len(rb) > longest {
		longest = len(rb)
	}
	return float64(prev[len(rb)]) / float64(longest)
}

func containsKanji(s string) bool {
	for _, r := range s {
		if unicode.Is(unicode.Scripts["Han"], r) {
			return true
		}
	}
	return false
}

// kataToHira 片假名→平假名（ぁ-ゖ 范围内平移 0x60）。
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
