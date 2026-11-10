package wordclip

import (
	"sort"
	"strings"
	"unicode"
)

const maxClips = 5

// 目标词匹配候选：同一句里可能有多个同词，取第一个实词 token。
type candidate struct {
	seg        Segment
	tokens     []Token
	matchStart int // rune 偏移，前端按此高亮
	matchEnd   int
	matchText  string
	mediaName  string
	score      float64
}

// skipSurface 判断该词面是否不适合做影视原声（句式占位、拉丁字母、过长等）。
func skipSurface(surface string) bool {
	s := strings.TrimSpace(surface)
	if s == "" || len([]rune(s)) > 15 {
		return true
	}
	if strings.ContainsAny(s, "〜~…○□- ・") {
		return true
	}
	for _, r := range s {
		if unicode.IsLetter(r) && r < 0x3000 {
			return true
		}
	}
	return false
}

// normReading 去掉音调标记（「もじ ①」→「もじ」），片假名转平假名，便于比对。
func normReading(reading string) string {
	r := strings.TrimSpace(reading)
	if i := strings.IndexAny(r, " \t　"); i >= 0 {
		r = r[:i]
	}
	var b strings.Builder
	for _, ch := range r {
		if ch >= 'ァ' && ch <= 'ヶ' {
			ch -= 0x60
		}
		b.WriteRune(ch)
	}
	return b.String()
}

// functionalToken 助词/助动词/接尾/记号等不做记忆例句的目标词：
// 这类词在台词里几乎只剩语法功能，截出来听不懂也记不住。
func functionalToken(t Token) bool {
	if t.Kind == "function" || t.Kind == "symbol" {
		return true
	}
	if t.Kind == "counter" {
		return false
	}
	for _, p := range []string{"助詞", "助動詞", "接続詞", "感動詞", "記号", "接尾", "接頭"} {
		if t.Pt == p || strings.Contains(t.PosLabel, p) {
			return true
		}
	}
	return false
}

func kataToHira(s string) string {
	var b strings.Builder
	for _, r := range s {
		if r >= 'ァ' && r <= 'ヶ' {
			r -= 0x60
		}
		b.WriteRune(r)
	}
	return b.String()
}

// findMatch 在句子的 token 流里找目标词：
// 原形/词面对上，或（假名读音对上且是实词）。返回 token 下标与是否命中。
func findMatch(seg Segment, surface, reading string) (int, bool) {
	surface = strings.TrimSpace(surface)
	norm := normReading(reading)
	tokens := seg.TextJa.Tokens
	for i, t := range tokens {
		if functionalToken(t) {
			continue
		}
		if t.S == surface || t.D == surface {
			return i, true
		}
		if norm != "" && kataToHira(t.R) == norm {
			return i, true
		}
	}
	// サ変動詞：词单写「勉強する」，台词里是「勉強」+「する」
	if strings.HasSuffix(surface, "する") && len([]rune(surface)) > 2 {
		base := strings.TrimSuffix(surface, "する")
		for i, t := range tokens {
			if functionalToken(t) {
				continue
			}
			if t.S == base || t.D == base {
				if i+1 < len(tokens) && tokens[i+1].S == "する" {
					return i, true
				}
				return i, true
			}
		}
	}
	return 0, false
}

func runeLen(s string) int { return len([]rune(s)) }

// hitRange 计算目标 token 在整句中的 rune 起止；token 流与正文不一致时回退找子串。
func hitRange(content string, tokens []Token, idx int) (int, int) {
	pos := 0
	for i, t := range tokens {
		l := runeLen(t.S)
		if i == idx {
			runes := []rune(content)
			if pos+l <= len(runes) && string(runes[pos:pos+l]) == t.S {
				return pos, pos + l
			}
			break
		}
		pos += l
	}
	if idx < len(tokens) {
		if r := strings.Index(content, tokens[idx].S); r >= 0 {
			start := runeLen(content[:r])
			return start, start + runeLen(tokens[idx].S)
		}
	}
	return 0, 0
}

func isContentKind(kind string) bool {
	switch kind {
	case "word", "compound", "inflected", "counter", "expression":
		return true
	}
	return false
}

func hasPredicate(tokens []Token) bool {
	for _, t := range tokens {
		if t.Kind == "inflected" || t.Pt == "動詞" || t.Pt == "形容詞" ||
			strings.Contains(t.PosLabel, "動詞") || strings.Contains(t.PosLabel, "形容詞") {
			return true
		}
	}
	return false
}

// scoreSegment 给候选句打分：先保证词形准确，再看句子完整、时长合适、内容充实。
func scoreSegment(seg Segment, rank int, surface string, idx int) float64 {
	t := seg.TextJa.Tokens[idx]
	content := strings.TrimSpace(seg.TextJa.Content)
	score := 100 - float64(rank)*2

	if t.S == surface {
		score += 18
	} else if t.D == surface {
		score += 10
	} else {
		score += 4 // 靠读音命中的同音词，保留但有错义风险，交给 LLM 校验
	}
	if strings.HasSuffix(content, "。") || strings.HasSuffix(content, "？") ||
		strings.HasSuffix(content, "！") || strings.HasSuffix(content, "!") {
		score += 8
	}
	n := runeLen(content)
	switch {
	case n >= 10 && n <= 28:
		score += 8
	case n < 10:
		score -= 2
	case n > 35:
		score -= 4
	}
	dur := seg.EndTimeMs - seg.StartTimeMs
	if dur >= 2000 && dur <= 8000 {
		score += 6
	}
	contentTokens := 0
	for _, tok := range seg.TextJa.Tokens {
		if isContentKind(tok.Kind) {
			contentTokens++
		}
	}
	if contentTokens >= 3 {
		score += 5
	}
	if strings.Contains(content, "、") || strings.Contains(content, "，") {
		score += 2
	}
	if hasPredicate(seg.TextJa.Tokens) {
		score += 6
	}
	if len(seg.TextJa.Tokens) > 30 {
		score -= 4
	}
	// 结尾停在助词上的多半是被截断的半句
	if n := len(seg.TextJa.Tokens); n > 0 {
		last := seg.TextJa.Tokens[n-1]
		if last.Pt == "助詞" || last.Pt == "助動詞" {
			score -= 6
		}
	}
	return score
}

// pickClips 从搜索结果里挑最多 5 条：词形精确、句子完整、媒体尽量分散。
func pickClips(surface, reading string, segs []Segment, media mediaSet) []candidate {
	cands := make([]candidate, 0, len(segs))
	seenText := map[string]bool{}
	for i, seg := range segs {
		if seg.Status != "" && seg.Status != "ACTIVE" {
			continue
		}
		content := strings.TrimSpace(seg.TextJa.Content)
		if content == "" || seg.Urls.VideoURL == "" || seg.Urls.ImageURL == "" {
			continue
		}
		// 歌词/被截断的句子不适合做例句：以《 开头的是歌词，含 " -" 的是断句
		if strings.HasPrefix(content, "《") || strings.Contains(content, " -") ||
			strings.HasSuffix(content, "-") {
			continue
		}
		// 结尾停在省略号的短句/无谓语片段多为半句（「きれい...」「まるで...」）；
		// 长句且带谓语的尾音未尽仍保留
		if strings.HasSuffix(content, "…") || strings.HasSuffix(content, "...") {
			if runeLen(content) < 10 || !hasPredicate(seg.TextJa.Tokens) {
				continue
			}
		}
		if seenText[content] {
			continue
		}
		idx, ok := findMatch(seg, surface, reading)
		if !ok {
			continue
		}
		start, end := hitRange(content, seg.TextJa.Tokens, idx)
		if end <= start {
			continue
		}
		seenText[content] = true
		cands = append(cands, candidate{
			seg:        seg,
			tokens:     seg.TextJa.Tokens,
			matchStart: start,
			matchEnd:   end,
			matchText:  seg.TextJa.Tokens[idx].S,
			mediaName:  media[seg.MediaPublicID].displayName(),
			score:      scoreSegment(seg, i, surface, idx),
		})
	}
	sort.SliceStable(cands, func(i, j int) bool { return cands[i].score > cands[j].score })

	// 两轮：第一轮每个作品最多 1 条，第二轮再补，避免 5 条全来自同一部剧
	selected := make([]candidate, 0, maxClips)
	mediaCount := map[string]int{}
	for pass := 0; pass < 2 && len(selected) < maxClips; pass++ {
		for _, c := range cands {
			if len(selected) >= maxClips {
				break
			}
			if pass == 0 && mediaCount[c.seg.MediaPublicID] > 0 {
				continue
			}
			dup := false
			for _, s := range selected {
				if s.seg.PublicID == c.seg.PublicID || s.seg.TextJa.Content == c.seg.TextJa.Content {
					dup = true
					break
				}
			}
			if dup {
				continue
			}
			mediaCount[c.seg.MediaPublicID]++
			selected = append(selected, c)
		}
	}
	return selected
}
