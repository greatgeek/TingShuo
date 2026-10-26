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
