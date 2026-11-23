package dict

import "testing"

func TestSanitizeExamplesDropsChineseMix(t *testing.T) {
	src := []zhExample{
		{Jp: "ユーチューバーが做的就是動画を投稿することだ。", Zh: "网络视频创作者做的就是发布视频。"},
		{Jp: "このユーチューバーは料理動画を投稿している。", Zh: "这位网络视频创作者正在发布料理视频。"},
		{Jp: "彼は毎日ユーチューバーとして動画を配信している。", Zh: "他每天作为视频博主发布视频。"},
	}
	out := sanitizeExamples(src)
	if len(out) != 2 {
		t.Fatalf("want 2 kept examples, got %d", len(out))
	}
	for _, ex := range out {
		if hasChineseOnly(ex.Jp) {
			t.Fatalf("chinese-only chars leaked: %s", ex.Jp)
		}
	}
}

func TestHasChineseOnly(t *testing.T) {
	cases := map[string]bool{
		"この部屋はとても明るいです。":   false,
		"目的を持って勉強している。":     false,
		"了解しました。":           false,
		"データを見直す時間がない。":     false,
		"ユーチューバーが做的就是動画を投稿することだ。": true,
		"我们明天一起去吧。":         true,
	}
	for s, want := range cases {
		if got := hasChineseOnly(s); got != want {
			t.Errorf("hasChineseOnly(%q)=%v want %v", s, got, want)
		}
	}
}