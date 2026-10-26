package generate

import (
	"encoding/json"
	"strings"
	"testing"
)

func validOutput() GenerationOutput {
	out := GenerationOutput{
		Materials: []MaterialGen{
			{ID: "m1", Type: "dialogue", Sentences: []SentenceGen{
				{ID: "s1", Text: "いらっしゃいませ。", Translation: "欢迎光临。"},
				{ID: "s2", Text: "これ、お願いします。", Translation: "我要买这个。"},
				{ID: "s3", Text: "全部で480円になります。", Translation: "一共 480 日元。"},
				{ID: "s4", Text: "精算してもいいですか。", Translation: "一起结账可以吗。"},
				{ID: "s5", Text: "かしこまりました。", Translation: "好的。"},
				{ID: "s6", Text: "ありがとうございます。", Translation: "谢谢。"},
				{ID: "s7", Text: "またお越しください。", Translation: "欢迎再来。"},
				{ID: "s8", Text: "失礼します。", Translation: "告辞了。"},
			}},
		},
		QAItems: []QAItemGen{
			{Prompt: "お会計はいくらでしたか。", Answer: "480円でした。"},
			{Prompt: "q2", Answer: "a2"}, {Prompt: "q3", Answer: "a3"},
			{Prompt: "q4", Answer: "a4"}, {Prompt: "q5", Answer: "a5"},
		},
		Substitutions: []SubstitutionGen{
			{WordID: "w1", PromptZh: "p1", Answer: "a1"},
			{WordID: "w2", PromptZh: "p2", Answer: "a2"},
			{WordID: "w3", PromptZh: "p3", Answer: "a3"},
			{WordID: "w4", PromptZh: "p4", Answer: "a4"},
			{WordID: "w5", PromptZh: "p5", Answer: "a5"},
		},
		WordOccurrence: map[string]WordOcc{
			"w1": {Sentences: []string{"s4"}},
			"w2": {Sentences: []string{"s1", "s7"}},
			"w3": {Sentences: []string{"s2"}},
			"w4": {Sentences: []string{"s3"}},
			"w5": {Sentences: []string{"s5"}},
		},
	}
	return out
}

func TestValidatePass(t *testing.T) {
	words := map[string]bool{"w1": false, "w2": true, "w3": false, "w4": false, "w5": false}
	if problems := validOutput().Validate(words); len(problems) > 0 {
		t.Fatalf("合法材料不应报错: %v", problems)
	}
}

func TestValidateMissingWord(t *testing.T) {
	out := validOutput()
	if problems := out.Validate(map[string]bool{"w1": false, "w2": true, "w3": false, "w4": false, "w5": false, "w6": false}); len(problems) == 0 {
		t.Fatal("缺少 w6 复现应报错")
	}
}

func TestValidateSkippedOK(t *testing.T) {
	out := validOutput()
	out.WordOccurrence["w1"] = WordOcc{Skipped: true, SkipReason: "专有名词不适合本场景"}
	if problems := out.Validate(map[string]bool{"w1": true, "w2": true, "w3": false, "w4": false, "w5": false}); len(problems) > 0 {
		t.Fatalf("skipped 且有原因应通过（重点词允许 skip）: %v", problems)
	}
}

func TestValidateBadSentenceRef(t *testing.T) {
	out := validOutput()
	out.WordOccurrence["w1"] = WordOcc{Sentences: []string{"s999"}}
	if problems := out.Validate(map[string]bool{"w1": true, "w2": true, "w3": false, "w4": false, "w5": false}); len(problems) == 0 {
		t.Fatal("引用不存在的句子应报错")
	}
}

func TestValidateKeyWordSingleOccurrence(t *testing.T) {
	out := validOutput()
	out.WordOccurrence["w2"] = WordOcc{Sentences: []string{"s1"}} // 重点词只出现 1 次
	problems := out.Validate(map[string]bool{"w1": false, "w2": true, "w3": false, "w4": false, "w5": false})
	t.Logf("problems: %v", problems)
	found := false
	for _, p := range problems {
		if strings.HasPrefix(p, "重点词") {
			found = true
		}
	}
	if !found {
		t.Fatal("重点词仅 1 次应产生提示性问题")
	}
}

func TestWordOccUnmarshal(t *testing.T) {
	var occ WordOcc
	if err := json.Unmarshal([]byte(`["s1","s2"]`), &occ); err != nil {
		t.Fatal(err)
	}
	if len(occ.Sentences) != 2 || occ.Skipped {
		t.Fatalf("数组形态解析错误: %+v", occ)
	}
	if err := json.Unmarshal([]byte(`"skipped: 专有名词"`), &occ); err != nil {
		t.Fatal(err)
	}
	if !occ.Skipped || occ.SkipReason != "专有名词" {
		t.Fatalf("skipped 形态解析错误: %+v", occ)
	}
}
