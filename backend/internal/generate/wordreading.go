// 读音校正：联想词由大模型给读音，偶有漏读（家電店→かいてん）。
// 词典读音不能直接当权威——单个词元的词典默认读音常有语境变体
// （大分 おおいた/だいぶ、唐揚げ とうあげ/からあげ），覆盖会改错。
// 因此只修“多成分全汉字名词组合、且词典拼接读音确比现存读音更长”的漏读。
package generate

import (
	"strings"
	"unicode"

	"github.com/ikawaha/kagome/v2/tokenizer"
)

// CorrectReading 返回校正后的读音；无法确证漏读时原样返回。
// 应用例：家電店 かいてん → かでんてん（家電+店，词典 5 拍 > 现存 4 拍）。
// 不动的例子：唐揚げ（单字组合但读音等长）、大分（单词，词典是地名读音）、
// 三つ星（含数词，变调）、オレンジ色（含非汉字成分）。
func CorrectReading(surface, stored string) string {
	if stored == "" || !containsKanji(surface) {
		return stored
	}
	tk, err := tokenizer.New(ipaDict)
	if err != nil {
		return stored
	}
	var toks []tokenizer.Token
	for _, m := range tk.Tokenize(surface) {
		if m.ID == tokenizer.BosEosID || m.Surface == "" {
			continue
		}
		toks = append(toks, m)
	}
	if len(toks) < 2 {
		return stored
	}
	var b strings.Builder
	for _, m := range toks {
		if !containsKanji(m.Surface) || isRubyNumeral(rubyTok{surface: m.Surface, pos: m.POS()}) {
			return stored
		}
		if !safeNounCompoundPOS(m.POS()) {
			return stored
		}
		r := wordReadingOf(m)
		if r == "" {
			return stored
		}
		b.WriteString(r)
	}
	dict := b.String()
	if !wordKana(dict) || len([]rune(dict)) <= len([]rune(stored)) {
		return stored
	}
	return dict
}

func wordReadingOf(m tokenizer.Token) string {
	r, ok := m.Reading()
	if !ok || r == "" {
		return ""
	}
	r = kataToHira(r)
	if !wordKana(r) {
		return ""
	}
	return r
}

// safeNounCompoundPOS 只放行不会变调的名词性词性：
// 接頭詞（お/ご/第）、名詞-一般/サ変接続、名詞-接尾-一般。
func safeNounCompoundPOS(pos []string) bool {
	p0, p1 := posAt(pos, 0), posAt(pos, 1)
	switch p0 {
	case "接頭詞":
		return p1 == "名詞接続"
	case "名詞":
		if p1 == "一般" || p1 == "サ変接続" {
			return true
		}
		return p1 == "接尾" && posAt(pos, 2) == "一般"
	}
	return false
}

func posAt(pos []string, i int) string {
	if i < len(pos) {
		return pos[i]
	}
	return ""
}

// wordKana 词级读音校验：平假名 + 长音符/中点；不含汉字、片假名、拉丁字母。
func wordKana(s string) bool {
	hasKana := false
	for _, r := range s {
		switch {
		case unicode.In(r, unicode.Hiragana):
			hasKana = true
		case r == 0x30FC || r == 0x30FB: // ー ・
		default:
			return false
		}
	}
	return hasKana
}
