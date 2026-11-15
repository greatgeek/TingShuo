package tts

import "testing"

func sp(s string) *string { return &s }

func TestParseRuby(t *testing.T) {
	in := [][]*string{{sp("きれいな"), nil}, {sp("町"), sp("まち")}}
	tokens, spoken, ok := parseRuby("きれいな町", in)
	if !ok || spoken != "きれいなまち" || len(tokens) != 2 {
		t.Fatalf("valid ruby: ok=%v spoken=%q tokens=%d", ok, spoken, len(tokens))
	}
	if tokens[1].Reading != "まち" {
		t.Fatalf("reading = %q", tokens[1].Reading)
	}

	// 表记拼接与 text 不一致：整组丢弃，避免念出与页面不一致的内容
	if _, _, ok := parseRuby("ちがう", in); ok {
		t.Fatal("surface mismatch should be rejected")
	}

	// 空注音：退回纯文本
	if _, _, ok := parseRuby("きれいな町", nil); ok {
		t.Fatal("empty ruby should be rejected")
	}

	// 单元素 token（无读音槽）合法，按表记展开
	tokens, spoken, ok = parseRuby("町", [][]*string{{sp("町")}})
	if !ok || spoken != "町" || len(tokens) != 1 || tokens[0].Reading != "" {
		t.Fatalf("bare token: ok=%v spoken=%q", ok, spoken)
	}
}
