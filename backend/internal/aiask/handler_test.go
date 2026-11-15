package aiask

import (
	"strings"
	"testing"
)

func TestBodyText(t *testing.T) {
	body := []byte(`[
		{"type":"p","text":"形容词修饰名词"},
		{"type":"item","term":"Ⅰ类","text":"A＋名词"},
		{"type":"ex","seq":1},
		{"type":"table","title":"表1","head":["时态","肯定"],"rows":[
			["现在","学生です。"],
			["过去",{"t":"学生でした。","say":"がくせいでした"}]
		]}
	]`)
	got := bodyText(body)
	for _, want := range []string{
		"形容词修饰名词", "Ⅰ类：A＋名词", "表1",
		"时态 | 肯定", "现在 | 学生です。", "过去 | 学生でした。",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("bodyText 缺少 %q，实际：\n%s", want, got)
		}
	}
	if strings.Contains(got, "がくせいでした") {
		t.Fatalf("表格单元格应取 t 而不是朗读用 say：\n%s", got)
	}
}

func TestBodyTextInvalid(t *testing.T) {
	if got := bodyText([]byte(`not json`)); got != "" {
		t.Fatalf("坏 JSON 应返回空串，实际 %q", got)
	}
}

func TestSentenceSystemWithWord(t *testing.T) {
	got := sentenceSystem("きれいな町", `[["きれいな",null],["町","まち"]]`, "干净漂亮的城市", "町", "まち", "城镇")
	for _, want := range []string{"，所属生词一并给出：", "句子：きれいな町", "生词：町（まち）城镇"} {
		if !strings.Contains(got, want) {
			t.Fatalf("句子提示词缺少 %q，实际：\n%s", want, got)
		}
	}
}

func TestSentenceSystemWithoutWord(t *testing.T) {
	got := sentenceSystem("白いパソコンと同い年です。", "（無）", "和白色电脑同一年。", "", "", "")
	if strings.Contains(got, "生词：") || strings.Contains(got, "所属生词") {
		t.Fatalf("无生词时不应出现生词行：\n%s", got)
	}
	if !strings.Contains(got, "中文：和白色电脑同一年。\n回答要求：") {
		t.Fatalf("中文行与回答要求之间不应有空行标题：\n%s", got)
	}
}

func TestGrammarSystem(t *testing.T) {
	g := &grammarCtx{
		Book:        "初级上",
		LessonTitle: "第11课 電気屋さんで",
		Section:     "grammar",
		Pattern:     "形容词修饰名词 ＜形容词的连体形＞",
		Formation:   "A＋名词",
		Body:        []byte(`[{"type":"p","text":"Ⅱ类形容词需加な"}]`),
		Examples:    []string{"白いパソコン（白色电脑）", "きれいな町（干净漂亮的城市）"},
	}
	got := g.system()
	for _, want := range []string{
		"《初级上》", "第11课 電気屋さんで", "语法解释",
		"形容词修饰名词 ＜形容词的连体形＞", "接续：A＋名词", "Ⅱ类形容词需加な",
		"白いパソコン（白色电脑）", "きれいな町（干净漂亮的城市）", "假名注音",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("system 提示词缺少 %q，实际：\n%s", want, got)
		}
	}
}

func TestGrammarSystemExpression(t *testing.T) {
	g := &grammarCtx{Book: "初级下", LessonTitle: "第18课 テスト", Section: "expression", Pattern: "病院で使う言葉"}
	got := g.system()
	if !strings.Contains(got, "表达扩展") {
		t.Fatalf("表达扩展标签缺失：\n%s", got)
	}
	if strings.Contains(got, "接续：") || strings.Contains(got, "例句：") {
		t.Fatalf("无接续/例句时不应出现空标题：\n%s", got)
	}
}
