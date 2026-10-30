// 句子/译文入库前校验：语言纯度、长度、译文存在且为中文。生成链路与 QA 工具共用。
package generate

import "regexp"

// simplifiedRe 简体中文特征：仅简体字形与中文标点。日文文本不应出现。
// 注意排除日文正当用字：写（写真）、被（被害）等不得入表。
var simplifiedRe = regexp.MustCompile(`[时给请帮们吗呢把很还从对为务备门变适业让连买卖关开东车马鱼鸟龙风飞见觉现，；“”‘’]`)

var hiraganaRe = regexp.MustCompile(`[ぁ-ん]`)
var hanRe = regexp.MustCompile(`[一-鿯]`)

// HasSimplifiedChinese 文本是否混入简体中文特征（/audio/say 护栏与 QA 共用）。
func HasSimplifiedChinese(s string) bool { return simplifiedRe.MatchString(s) }

// ValidSentence 生成句子落库门槛：非空、长度上限、日文纯净、译文为中文且无假名泄漏。
func ValidSentence(text, translation string) bool {
	if text == "" || translation == "" {
		return false
	}
	if len([]rune(text)) > 120 {
		return false
	}
	if HasSimplifiedChinese(text) {
		return false
	}
	if !hanRe.MatchString(translation) || hiraganaRe.MatchString(translation) {
		return false
	}
	return true
}
