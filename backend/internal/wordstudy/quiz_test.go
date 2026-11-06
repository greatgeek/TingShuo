package wordstudy

import (
	"encoding/json"
	"testing"
)

func ruby(t *testing.T, pairs [][2]string) json.RawMessage {
	t.Helper()
	out := []any{}
	for _, p := range pairs {
		var r *string
		if p[1] != "" {
			v := p[1]
			r = &v
		}
		out = append(out, [2]*string{&p[0], r})
	}
	b, err := json.Marshal(out)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// 目标词跨两个 token（コーヒー 拆成 コー/ヒー）也要整体空白，且空白不带读音。
func TestBlankTokensMergesOverlap(t *testing.T) {
	raw := ruby(t, [][2]string{{"彼", "かれ"}, {"は", ""}, {"コー", ""}, {"ヒー", ""}, {"を", ""}, {"飲", "の"}, {"む", ""}})
	got := blankTokens(raw, "彼はコーヒーを飲む", "コーヒー")
	if got == nil {
		t.Fatal("nil tokens")
	}
	blanks, texts := 0, ""
	for _, tk := range got {
		if tk.Blank {
			blanks++
			if tk.Reading != nil {
				t.Errorf("空白 token 不应带读音: %v", *tk.Reading)
			}
			continue
		}
		texts += tk.Text
		if tk.Text == "コー" || tk.Text == "ヒー" {
			t.Errorf("目标词残片漏出: %q", tk.Text)
		}
	}
	if blanks != 1 {
		t.Errorf("空白 token 数 = %d, 想 1", blanks)
	}
	if texts != "彼はを飲む" {
		t.Errorf("其余 token 拼接 = %q", texts)
	}
}

func TestBlankTokensMissingSurface(t *testing.T) {
	raw := ruby(t, [][2]string{{"今日", "きょう"}, {"は", ""}})
	if got := blankTokens(raw, "今日は晴天", "コーヒー"); got != nil {
		t.Errorf("词面不在句中应返回 nil, 得 %v", got)
	}
}

// 句式条目（〜さん/…）不进听力通道：念出来不明所以（2026-11-06 报障）
func TestPatternWord(t *testing.T) {
	for _, s := range []string{"〜さんのおかげです", "〜てください", "…でもいい"} {
		if !patternWord(s) {
			t.Errorf("%q 应判为句式条目", s)
		}
	}
	for _, s := range []string{"切符", "増える", "何曜日"} {
		if patternWord(s) {
			t.Errorf("%q 不应判为句式条目", s)
		}
	}
}

func TestBlurHint(t *testing.T) {
	tr, mg := "他每天早上喝咖啡。", "咖啡"
	h := blurHint(&tr, &mg)
	if h == nil || *h != "他每天早上喝○○。" {
		t.Fatalf("hint = %v", h)
	}
	// 翻译里不含词义 → 不给提示，宁缺勿泄
	tr2 := "他起得很早。"
	if h2 := blurHint(&tr2, &mg); h2 != nil {
		t.Errorf("不含词义应为 nil, 得 %q", *h2)
	}
	if blurHint(nil, &mg) != nil || blurHint(&tr, nil) != nil {
		t.Error("缺翻译或缺词义应为 nil")
	}
}
