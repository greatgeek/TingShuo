package notebook

import (
	"sync"

	"github.com/greatgeek/TingShuo/backend/internal/generate"
)

// wordRubyTokens 词级逐块振假名：kagome 只负责切块，读音以生词本存的 reading 为准
// （kagome 词典音随上下文偏，如何=なに/なん，直接采用会和本子记的对不上）。
// 多块时按各块词典音的假名字数把 reading 切开；假名尾（送り仮名）按字面消耗；
// 任何对不齐的情况退化为整词一块，宁整勿错。
var wordRubyCache sync.Map

func wordRubyTokens(surface, reading string) [][]any {
	if reading == "" || reading == surface {
		return nil
	}
	key := surface + "\x00" + reading
	if v, ok := wordRubyCache.Load(key); ok {
		return v.([][]any)
	}
	out := buildWordRuby(surface, reading)
	wordRubyCache.Store(key, out)
	return out
}

func buildWordRuby(surface, reading string) [][]any {
	whole := splitOkurigana([]any{surface, reading})
	toks := generate.BuildRubyTokens(surface)
	if len(toks) <= 1 {
		return whole
	}
	runes := []rune(reading)
	off := 0
	out := make([][]any, 0, len(toks))
	for _, t := range toks {
		s, _ := t[0].(string)
		rd, _ := t[1].(string)
		if n := len([]rune(rd)); n > 0 {
			if off+n > len(runes) {
				return whole
			}
			out = append(out, []any{s, string(runes[off : off+n])})
			off += n
			continue
		}
		// 无词典音的块（假名/标点）：按字面消耗 reading 对应段
		n := len([]rune(s))
		if off+n > len(runes) || string(runes[off:off+n]) != s {
			return whole
		}
		out = append(out, []any{s, nil})
		off += n
	}
	if off != len(runes) {
		return whole
	}
	refined := make([][]any, 0, len(out))
	for _, t := range out {
		refined = append(refined, splitOkurigana(t)...)
	}
	return refined
}

// splitOkurigana 送假名剥离：表面形与读音的公共假名尾部不属于汉字注音。
// 食べる/たべる → 食(た) + べる；剥不出（大人/おとな）或剥空就保持整块。
func splitOkurigana(t []any) [][]any {
	s, _ := t[0].(string)
	r, _ := t[1].(string)
	if r == "" {
		return [][]any{t}
	}
	sr, rr := []rune(s), []rune(r)
	i, j := len(sr), len(rr)
	for i > 0 && j > 0 && sr[i-1] == rr[j-1] && isKana(sr[i-1]) {
		i--
		j--
	}
	if i == 0 || j == 0 || i == len(sr) || !containsKanjiRun(sr[:i]) {
		return [][]any{t}
	}
	return [][]any{{string(sr[:i]), string(rr[:j])}, {string(sr[i:]), nil}}
}

func isKana(r rune) bool {
	return (r >= 0x3041 && r <= 0x3096) || (r >= 0x30A1 && r <= 0x30FA)
}

func containsKanjiRun(rs []rune) bool {
	for _, r := range rs {
		if r >= 0x4E00 && r <= 0x9FFF {
			return true
		}
	}
	return false
}
