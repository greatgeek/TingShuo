// 振り仮名增强（FR-2.12）：kagome 分词 + 词典读音 → ruby_tokens；
// LLM 整句假名仅作交叉校验参考，不一致记 warning，不阻塞落库。
package generate

import (
	"strings"
	"unicode"

	ipa "github.com/ikawaha/kagome-dict/ipa"
	"github.com/ikawaha/kagome/v2/tokenizer"
)

// ipaDict 全局加载一次（IPA 词典约 50MB，进程内共享，只读安全）。
var ipaDict = ipa.Dict()

// readingFix 上下文读音纠正：kagome 按词元取词典读音，个别组合会误读
//（「この後」被拆成 この＋後(のち)，实读 このあと）。键为 [前一词元, 当前词元]。
var readingFix = map[[2]string]string{
	{"この", "後"}: "あと",
}

// BuildRubyTokens 对句子分词并输出振り仮名结构：
// [["会計","かいけい"],["を",null],...]——含汉字词带平假名读音，其余 null。
func BuildRubyTokens(text string) [][]any {
	t, err := tokenizer.New(ipaDict)
	if err != nil {
		// 词典加载失败时退化为纯文本 token（振り仮名缺失但不阻塞落库）
		return [][]any{{text, nil}}
	}
	type tok struct {
		surface string
		reading string
	}
	toks := make([]tok, 0, 16)
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
			toks = append(toks, tok{surface, kataToHira(reading)})
		} else {
			toks = append(toks, tok{surface, ""})
		}
	}
	for i := 1; i < len(toks); i++ {
		if r, ok := readingFix[[2]string{toks[i-1].surface, toks[i].surface}]; ok {
			toks[i].reading = r
		}
	}
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
