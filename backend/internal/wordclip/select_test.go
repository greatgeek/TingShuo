package wordclip

import "testing"

func testSegment(text string, tokens []Token, dur int) Segment {
	var s Segment
	s.Status = "ACTIVE"
	s.TextJa.Content = text
	s.TextJa.Tokens = tokens
	s.EndTimeMs = dur
	s.Urls.ImageURL = "https://example.com/i.jpg"
	s.Urls.VideoURL = "https://example.com/v.mp4"
	return s
}

func TestPickClipsShortSentenceFirst(t *testing.T) {
	target := Token{S: "週末", D: "週末", Kind: "word", Pt: "名詞"}
	particle := Token{S: "は", Kind: "function", Pt: "助詞"}
	verb := Token{S: "手伝え", D: "手伝う", Kind: "inflected", Pt: "動詞"}
	short := testSegment("週末は手伝えよ!", []Token{target, particle, verb, {S: "よ", Kind: "function", Pt: "助詞"}, {S: "!"}}, 3000)
	long := testSegment("週末の予定は今日はとても長い話をしますから気をつけてください", []Token{target, {S: "の", Kind: "function", Pt: "助詞"}, verb}, 6000)

	got := pickClips("週末", "", []Segment{long, short}, mediaSet{}, pickOptions{limit: 1})
	if len(got) != 1 || got[0].seg.TextJa.Content != short.TextJa.Content {
		t.Fatalf("expected short sentence first, got %+v", got)
	}
}

func TestPickClipsDropsShortFragments(t *testing.T) {
	frag := testSegment("卒業試験", []Token{
		{S: "卒業", D: "卒業", Kind: "word", Pt: "名詞"},
		{S: "試験", D: "試験", Kind: "word", Pt: "名詞"},
	}, 2000)
	full := testSegment("試験は来週の月曜日にあります", []Token{
		{S: "試験", D: "試験", Kind: "word", Pt: "名詞"},
		{S: "は", Kind: "function", Pt: "助詞"},
		{S: "あります", D: "ある", Kind: "inflected", Pt: "動詞"},
	}, 3000)

	got := pickClips("試験", "", []Segment{frag, full}, mediaSet{}, pickOptions{limit: 6})
	if len(got) != 1 || got[0].seg.TextJa.Content != full.TextJa.Content {
		t.Fatalf("fragment should be dropped, got %+v", got)
	}
}

func TestPickClipsExcludesCachedSegments(t *testing.T) {
	target := Token{S: "週末", D: "週末", Kind: "word", Pt: "名詞"}
	verb := Token{S: "行く", D: "行く", Kind: "inflected", Pt: "動詞"}
	a := testSegment("週末は映画に行く", []Token{target, verb}, 3000)
	a.PublicID = "seg-a"
	b := testSegment("週末は買い物に行く", []Token{target, verb}, 3000)
	b.PublicID = "seg-b"

	got := pickClips("週末", "", []Segment{a, b}, mediaSet{}, pickOptions{
		limit:      6,
		excludeSeg: map[string]bool{"seg-a": true},
	})
	if len(got) != 1 || got[0].seg.PublicID != "seg-b" {
		t.Fatalf("cached segment should be excluded, got %+v", got)
	}
}
