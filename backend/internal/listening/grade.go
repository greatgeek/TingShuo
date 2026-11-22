package listening

import (
	"regexp"
	"strings"
	"unicode"
)

var digitSeq = regexp.MustCompile(`[0-9]+`)

var kanjiDigits = map[rune]rune{
	'〇': '0', '一': '1', '二': '2', '三': '3', '四': '4',
	'五': '5', '六': '6', '七': '7', '八': '8', '九': '9',
}

// foldAnswer 让填空判分容忍全角/空格/标点/汉字数字与句尾です。
func foldAnswer(s string) string {
	var b strings.Builder
	for _, r := range s {
		if r >= 0xFF01 && r <= 0xFF5E {
			r -= 0xFEE0
		}
		if r == 0x3000 || unicode.IsSpace(r) {
			continue
		}
		if strings.ContainsRune("。、，．,.!！?？「」『』（）()・…ー〜~-—–−‐‑／/：:；;", r) {
			continue
		}
		if d, ok := kanjiDigits[r]; ok {
			r = d
		}
		b.WriteRune(r)
	}
	out := b.String()
	out = strings.TrimSuffix(out, "でした")
	out = strings.TrimSuffix(out, "です")
	return out
}

func isDigits(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

// answerOK 支持 "|" 分隔的备选答案；纯数字答案按用户输入中的第一段数字比对。
func answerOK(canonical, your string) bool {
	your = strings.TrimSpace(your)
	if your == "" {
		return false
	}
	y := foldAnswer(your)
	for _, alt := range strings.Split(canonical, "|") {
		alt = strings.TrimSpace(alt)
		if alt == "" {
			continue
		}
		folded := foldAnswer(alt)
		if folded == y {
			return true
		}
		if isDigits(folded) && digitSeq.FindString(y) == folded {
			return true
		}
	}
	return false
}