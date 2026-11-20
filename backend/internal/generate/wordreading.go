// 读音校正：联想词由大模型给读音，偶有按单个汉字生拼（好物→このもの、家電→かいでん）。
// 词典默认读音也不能无条件作准——IPADIC 对一部分词只收语境变体
// （大分 おおいた/だいぶ、その後 そのご/そのあと），硬覆盖会改错。
// 因此先验证现存读音是否是词典可解释的读法：是则保留，否则按词典重读。
package generate

import (
	"strings"
	"unicode"

	"github.com/ikawaha/kagome/v2/tokenizer"
)

// keepReading 词典未收、但确实常用的读法：现存读音是这些时保持。
// 这些词的词典默认读音（大分→おおいた、一昨年→いっさくねん、君→きみ）
// 在联想语境下不是要学的读音，不能拿词典硬覆盖。
var keepReading = map[string]string{
	"大分":  "だいぶ",
	"一昨年": "おととし",
	"市場":  "いちば",
	"何時":  "なんじ",
	"唐揚げ": "からあげ",
	"一人":  "ひとり",
	"鶏肉":  "とりにく",
	"君":   "くん",
	"時":   "じ",
	"分":   "ふん",
	"度度":  "たびたび",
	"その後": "そのあと",
}

// CorrectReading 返回校正后的读音；无法确证时原样返回。
// 单成分词：现存读音在词典该词条的读音里 → 保留；否则用词典读音
// （好物 このもの→こうぶつ、白色 しろいろ 保留——词典收 シロイロ/ハクショク）。
// 多成分词：现存读音能由各词元的词典读音拼出 → 保留（化粧品会社 けしょうひんがいしゃ
// 用 会社 ガイシャ 拼得出）；拼不出且是全汉字名词组合 → 用词典拼接
// （家電店 かいてん→かでんてん、同一の どういちの→どういつの）。
func CorrectReading(surface, stored string) string {
	if stored == "" || !containsKanji(surface) {
		return stored
	}
	storedHira := kataToHira(stored)
	if keep, ok := keepReading[surface]; ok && storedHira == keep {
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
	if len(toks) == 0 {
		return stored
	}

	// 单成分：词典该词条的全部读音均可信
	if len(toks) == 1 {
		for _, r := range dictReadings(toks[0].Surface) {
			if r == storedHira {
				return stored
			}
		}
		r := wordReadingOf(toks[0])
		if r == "" || r == storedHira {
			return stored
		}
		return r
	}

	// 多成分：现存读音若能由各词元词典读音拼出，则是合法读法
	if match, known := producibleReading(storedHira, toks); known && match {
		return stored
	}

	// 多成分：只放行全汉字名词组合，末尾可带一个「の」（同一の→どういつの）；
	// 含假名/数词/动词成分的一律不动，避免变调与惯用读法被覆盖。
	var b strings.Builder
	for i, m := range toks {
		if !containsKanji(m.Surface) {
			if m.Surface == "の" && i == len(toks)-1 {
				b.WriteString("の")
				continue
			}
			return stored
		}
		if isRubyNumeral(rubyTok{surface: m.Surface, pos: m.POS()}) || !safeNounCompoundPOS(m.POS()) {
			return stored
		}
		r := wordReadingOf(m)
		if r == "" {
			return stored
		}
		b.WriteString(r)
	}
	dict := b.String()
	if !wordKana(dict) || storedHira == dict {
		return stored
	}
	return dict
}

// dictReadings 返回 IPA 词典中该词形的全部已知读音（平假名）。
func dictReadings(surface string) []string {
	ids := ipaDict.Index.Search(surface)
	ri, ok := ipaDict.ContentsMeta["_reading"]
	if len(ids) == 0 || !ok {
		return nil
	}
	out := make([]string, 0, len(ids))
	for _, id := range ids {
		if id < 0 || id >= len(ipaDict.Contents) || id >= len(ipaDict.POSTable.POSs) {
			continue
		}
		ci := int(ri) - len(ipaDict.POSTable.POSs[id])
		row := ipaDict.Contents[id]
		if ci < 0 || ci >= len(row) {
			continue
		}
		if r := kataToHira(row[ci]); wordKana(r) {
			out = append(out, r)
		}
	}
	return out
}

// producibleReading 判断读音能否由各词元的词典读音顺序拼出；
// known=false 表示有含汉字的词元在词典里查不到，无法验证。
func producibleReading(stored string, toks []tokenizer.Token) (match, known bool) {
	known = true
	var rec func(i int, rest string) bool
	rec = func(i int, rest string) bool {
		if i == len(toks) {
			return rest == ""
		}
		rs := dictReadings(toks[i].Surface)
		if !containsKanji(toks[i].Surface) {
			rs = []string{kataToHira(toks[i].Surface)}
		} else if len(rs) == 0 {
			known = false
		}
		for _, r := range rs {
			if strings.HasPrefix(rest, r) && rec(i+1, rest[len(r):]) {
				return true
			}
		}
		return false
	}
	return rec(0, stored), known
}

// IsJapaneseWord 判断词形是否为日语词典可确证的词：每个含汉字的词元
// 都必须有词典读音。中文词里的简体字（其/别/试）在日语词典中没有读音，
// 会被拒绝（尤其是、特别、考试）；假名词不受影响。
func IsJapaneseWord(surface string) bool {
	if surface == "" {
		return false
	}
	tk, err := tokenizer.New(ipaDict)
	if err != nil {
		return true // 词典不可用时放行，不阻塞展示
	}
	for _, m := range tk.Tokenize(surface) {
		if m.ID == tokenizer.BosEosID || m.Surface == "" || !containsKanji(m.Surface) {
			continue
		}
		if r, ok := m.Reading(); !ok || r == "" {
			return false
		}
	}
	return true
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
// 接頭詞（お/ご/第）、名詞-一般/サ変接続/固有名詞、名詞-接尾-一般。
// 固有名詞（本地 ホンジ）读音固定，参与拼接不会误读。
func safeNounCompoundPOS(pos []string) bool {
	p0, p1 := posAt(pos, 0), posAt(pos, 1)
	switch p0 {
	case "接頭詞":
		return p1 == "名詞接続"
	case "名詞":
		if p1 == "一般" || p1 == "サ変接続" || p1 == "固有名詞" {
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
