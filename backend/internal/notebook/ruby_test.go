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

// 送り仮名对不齐（词典音字数和 reading 不符）→ 整词一块，宁整勿错
func TestBuildWordRubyFallbackWhole(t *testing.T) {
	got := buildWordRuby("多い", "おおい")
	if len(got) != 1 || got[0][1] != "おおい" {
		t.Errorf("应整词一块: %v", got)
	}
	// 单 token 词直接整块
	if g2 := buildWordRuby("切符", "きっぷ"); len(g2) != 1 || g2[0][1] != "きっぷ" {
		t.Errorf("单块词应整块: %v", g2)
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
