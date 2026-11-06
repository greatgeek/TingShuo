package wordstudy

import "testing"

func TestPatternTail(t *testing.T) {
	if got := patternTail("〜さんのおかげです"); got != "さんのおかげです" {
		t.Errorf("tail = %q", got)
	}
	if got := patternTail("切符"); got != "切符" {
		t.Errorf("非句式词 tail 应原样: %q", got)
	}
}

func TestValidPatternSent(t *testing.T) {
	tail := "さんのおかげです"
	ok := patternSent{Text: "今回の成功は田中さんのおかげです。", Translation: "这次的成功多亏了田中。"}
	if err := validPatternSent(ok, tail); err != nil {
		t.Errorf("合格句被拒: %v", err)
	}
	if err := validPatternSent(patternSent{Text: "〜さんのおかげです。", Translation: "含占位符"}, tail); err == nil {
		t.Error("含 〜 的句子应被拒")
	}
	if err := validPatternSent(patternSent{Text: "田中さんのおかげです。", Translation: ""}, tail); err == nil {
		t.Error("缺翻译应被拒")
	}
	if err := validPatternSent(patternSent{Text: "今回の成功は田中さんのおかげです。", Translation: "田中さんのおかげ"}, tail); err == nil {
		t.Error("翻译混假名应被拒")
	}
	if err := validPatternSent(patternSent{Text: "田中さんです。", Translation: "句式部分不在句里"}, tail); err == nil {
		t.Error("不含句式部分应被拒")
	}
}
