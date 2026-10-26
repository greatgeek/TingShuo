package lesson

import (
	"strings"
	"testing"
)

func mk(surface string) WordInput { return WordInput{Surface: surface} }

func TestValidateWordsCount(t *testing.T) {
	// 少于 5 个
	if _, e := ValidateWords([]WordInput{mk("精算"), mk("満員"), mk("預ける")}); e == nil {
		t.Fatal("3 个词应报 WORD_COUNT_INVALID")
	} else if !strings.Contains(e.Code, "WORD_COUNT_INVALID") {
		t.Fatalf("错误码应为 WORD_COUNT_INVALID，得到 %s", e.Code)
	}

	// 恰好 5 个（含空行跳过后仍够）
	words := []WordInput{mk("精算"), mk("満員"), mk("預ける"), {}, mk("精算x"), mk("駅")}
	got, e := ValidateWords(words)
	if e != nil {
		t.Fatalf("5 个有效词应通过: %v", e)
	}
	if len(got) != 5 {
		t.Fatalf("空行应被跳过，得到 %d 个", len(got))
	}

	// 超过 40 个
	tooMany := make([]WordInput, 41)
	for i := range tooMany {
		tooMany[i] = mk(string(rune('あ' + i)))
	}
	if _, e := ValidateWords(tooMany); e == nil {
		t.Fatal("41 个词应报错")
	}
}

func TestValidateWordsDedup(t *testing.T) {
	words := []WordInput{
		{Surface: "精算", Reading: "せいさん", Meaning: "旧释义"},
		{Surface: "満員", Reading: ""},
		{Surface: "  精算  ", Reading: "せいさん", Meaning: "新释义"}, // 同词去空白后覆盖
	}
	got, e := ValidateWords(append(words, mk("預ける"), mk("駅"), mk("切符")))
	if e != nil {
		t.Fatal(e)
	}
	if len(got) != 5 {
		t.Fatalf("重复词应合并为 1，得到 %d", len(got))
	}
	for _, w := range got {
		if w.Surface == "精算" && w.Meaning != "新释义" {
			t.Fatal("后出现的重复词应覆盖先出现的")
		}
	}
}

func TestValidateWordsReadingNorm(t *testing.T) {
	// Q-002 落地：reading 空白 → 空串（入库参与 UNIQUE 而非 NULL）
	words := []WordInput{
		{Surface: "a", Reading: "  "},
		{Surface: "b", Reading: " まんいん "},
		mk("c"), mk("d"), mk("e"),
	}
	got, e := ValidateWords(words)
	if e != nil {
		t.Fatal(e)
	}
	if got[0].Reading != "" {
		t.Fatalf("空白 reading 应归一为空串，得到 %q", got[0].Reading)
	}
	if got[1].Reading != "まんいん" {
		t.Fatalf("reading 应去首尾空白，得到 %q", got[1].Reading)
	}
}
