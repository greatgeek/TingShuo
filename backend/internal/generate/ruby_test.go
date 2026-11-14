package generate

import (
	"strings"
	"testing"
)

func TestBuildRubyTokens(t *testing.T) {
	ruby := BuildRubyTokens("全部で480円になります。")
	if len(ruby) == 0 {
		t.Fatal("应产出至少一个 token")
	}
	// BOS/EOS 占位词元必须被过滤（Surface 为字面量 "BOS"/"EOS"）
	for _, tok := range ruby {
		if s, ok := tok[0].(string); ok && (s == "BOS" || s == "EOS") {
			t.Fatalf("BOS/EOS 词元泄漏到振り仮名: %v", ruby)
		}
	}
	// 找「円」或「全」等含汉字词元的读音
	foundRuby := false
	for _, tok := range ruby {
		if len(tok) == 2 {
			if s, ok := tok[1].(string); ok && s != "" {
				foundRuby = true
				t.Logf("token: %v %v", tok[0], s)
			}
		}
	}
	if !foundRuby {
		t.Fatal("含汉字句子应至少有一个带读音的词元")
	}
	// 平假名词元应为 null
	ruby2 := BuildRubyTokens("これ、お願いします。")
	for _, tok := range ruby2 {
		if s, ok := tok[0].(string); ok && !containsKanji(s) {
			if v, ok2 := tok[1].(string); ok2 && v != "" {
				t.Fatalf("不含汉字的词元应为 nil，得到 %q", v)
			}
		}
	}
}

func TestReading(t *testing.T) {
	cases := map[string]string{
		"色々":  "いろいろ",
		"中村":  "なかむら",
		"かに座": "かにざ",
	}
	for in, want := range cases {
		if got := Reading(in); got != want {
			t.Fatalf("Reading(%q) = %q, want %q", in, got, want)
		}
	}
	// 课文写假名、用户写汉字：整句读音应一致（听写判对的服务端兜底）
	base := Reading("いえ、中村さんのおかげですよ。いろいろありがとう。")
	alt := Reading("いえ、なかむらさんのおかげですよ。色々ありがとう。")
	if base == "" || base != alt {
		t.Fatalf("整句读音不一致:\nbase=%q\nalt =%q", base, alt)
	}
}

func TestLearningWordChunks(t *testing.T) {
	segs := func(text string) []string {
		var out []string
		for _, tok := range BuildRubyTokens(text) {
			out = append(out, tok[0].(string))
		}
		return out
	}
	cases := []struct {
		text string
		want string
	}{
		{"重さはどれくらいですか。", "重さ｜は｜どれくらい｜です｜か｜。"},
		{"一時的にここに預けることはできますか。", "一時的｜に｜ここ｜に｜預ける｜こと｜は｜できます｜か｜。"},
		{"部屋全体の掃除と洗濯をします。", "部屋全体｜の｜掃除｜と｜洗濯｜を｜します｜。"},
		{"ジョージさんは経済学部の3年生です。", "ジョージさん｜は｜経済学部｜の｜3｜年生｜です｜。"},
		{"誕生日パーティーをしましょう！", "誕生日パーティー｜を｜しましょう｜！"},
	}
	for _, c := range cases {
		if got := strings.Join(segs(c.text), "｜"); got != c.want {
			t.Fatalf("seg(%q)\n got %q\nwant %q", c.text, got, c.want)
		}
	}
}

func TestDateMonthReading(t *testing.T) {
	cases := map[string]string{
		"4月は桜が咲きます。": "4がつはさくらがさきます。",
		"何月に行きますか。":  "なんがつにいきますか。",
		"月がきれいです。":   "つきがきれいです。",
	}
	for text, want := range cases {
		if got := Reading(text); got != want {
			t.Fatalf("Reading(%q) = %q, want %q", text, got, want)
		}
	}
}

func TestTimeReading(t *testing.T) {
	cases := map[string]string{
		"地下鉄がなかった時は、車がほしかったです。": "ちかてつがなかったときは、くるまがほしかったです。",
		"小さい時は、まだ日本語を習っていなかった。": "ちいさいときは、まだにほんごをならっていなかった。",
		"3時に駅で待ち合わせましょう。":       "3じにえきでまちあわせましょう。",
		"何時ですか。": "なんじですか。",
	}
	for text, want := range cases {
		if got := Reading(text); got != want {
			t.Fatalf("Reading(%q) = %q, want %q", text, got, want)
		}
	}
}

func TestKataToHira(t *testing.T) {
	if got := kataToHira("カイケイ"); got != "かいけい" {
		t.Fatalf("片→平转换错误: %q", got)
	}
	if got := kataToHira("まんいん"); got != "まんいん" {
		t.Fatalf("平假名应原样保留: %q", got)
	}
}

func TestKanaCrossCheck(t *testing.T) {
	ruby := BuildRubyTokens("全部で480円になります。")
	// 用 kagome 自己的读音拼回去 → 应该高一致率
	high := KanaCrossCheck(ruby, "ぜんぶでよんひゃくはちじゅうえんになります。")
	low := KanaCrossCheck(ruby, "たべものがすきです。")
	if high <= low {
		t.Fatalf("正确假名的相似度应高于无关串: %.2f vs %.2f", high, low)
	}
}

func TestNormalizeKana(t *testing.T) {
	if got := normalizeKana("カイケイ"); got != "かいけい" {
		t.Fatalf("片假名应转平假名: %q", got)
	}
	if got := normalizeKana("ぜんぶ で"); strings.ContainsAny(got, " \t") {
		t.Fatalf("空白应被移除: %q", got)
	}
}
