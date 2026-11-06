package notebook

import "testing"

// kagome 切块 + 本子读音切分：何曜日 → 何=なん / 曜日=ようび（词典音 なに 不能采用）
func TestBuildWordRubySplitsByKagomeBounds(t *testing.T) {
	got := buildWordRuby("何曜日", "なんようび")
	if len(got) != 2 {
		t.Fatalf("块数 = %d, 想 2: %v", len(got), got)
	}
	if got[0][0] != "何" || got[0][1] != "なん" || got[1][0] != "曜日" || got[1][1] != "ようび" {
		t.Errorf("切分错误: %v", got)
	}
}

// 送り仮名对不齐时靠剥离兜底：多い/おおい → 多(おお)+い；单 token 词直接整块
func TestBuildWordRubyFallbackWhole(t *testing.T) {
	got := buildWordRuby("多い", "おおい")
	if len(got) != 2 || got[0][0] != "多" || got[0][1] != "おお" || got[1][0] != "い" {
		t.Errorf("多い 应剥送假名: %v", got)
	}
	// 单 token 词直接整块
	if g2 := buildWordRuby("切符", "きっぷ"); len(g2) != 1 || g2[0][1] != "きっぷ" {
		t.Errorf("单块词应整块: %v", g2)
	}
}

// 送假名剥离：食べる/たべる → 食(た)+べる；大人/おとな 剥不出保持整块
func TestSplitOkurigana(t *testing.T) {
	got := buildWordRuby("食べる", "たべる")
	if len(got) != 2 || got[0][0] != "食" || got[0][1] != "た" || got[1][0] != "べる" || got[1][1] != nil {
		t.Errorf("食べる 应剥送假名: %v", got)
	}
	if g2 := buildWordRuby("増える", "ふえる"); len(g2) != 2 || g2[0][0] != "増" || g2[0][1] != "ふ" || g2[1][0] != "える" {
		t.Errorf("増える 应剥送假名: %v", g2)
	}
	if g3 := buildWordRuby("大人", "おとな"); len(g3) != 1 || g3[0][1] != "おとな" {
		t.Errorf("大人 应整块: %v", g3)
	}
	if g4 := buildWordRuby("切符", "きっぷ"); len(g4) != 1 || g4[0][1] != "きっぷ" {
		t.Errorf("切符 应整块: %v", g4)
	}
}

func TestWordRubyCacheAndNil(t *testing.T) {
	if wordRubyTokens("切符", "") != nil {
		t.Error("空读音应为 nil")
	}
	if wordRubyTokens("切符", "切符") != nil {
		t.Error("读音=词面应为 nil")
	}
	a := wordRubyTokens("何曜日", "なんようび")
	b := wordRubyTokens("何曜日", "なんようび")
	if len(a) != len(b) {
		t.Error("缓存应返回同一结果")
	}
}
