package podcast

import (
	"strings"
	"testing"
)

func TestParseLines(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want int
	}{
		{"标准对象", `{"lines":[{"role":"a","text":"你好"},{"role":"b","text":"こんにちは"}]}`, 2},
		{"代码围栏", "```json\n{\"lines\":[{\"role\":\"a\",\"text\":\"你好\"}]}\n```", 1},
		{"两个对象拼接", `{"lines":[{"role":"a","text":"一"}]}, {"lines":[{"role":"b","text":"二"}]}`, 2},
		{"裸数组", `[{"role":"a","text":"一"},{"role":"b","text":"二"}]`, 2},
		{"文本内含花括号", `{"lines":[{"role":"a","text":"「{」这个符号"}]}`, 1},
	}
	for _, c := range cases {
		got, err := parseLines(c.in)
		if err != nil {
			t.Errorf("%s: 解析失败 %v", c.name, err)
			continue
		}
		if len(got) != c.want {
			t.Errorf("%s: 得到 %d 行，想要 %d 行", c.name, len(got), c.want)
		}
	}
	if _, err := parseLines("今天天气不错，没有 JSON"); err == nil {
		t.Error("无 JSON 时应报错")
	}
}

func TestIsJaLine(t *testing.T) {
	ja := []string{
		"今月は外食が多かったです。",
		"約束があります。",
		"しょくひ。",
		"財布の中に千円しかありません。",
		"私は中国人です。",   // 汉字多于假名的纯日语也要认出来
		"李さんは、",       // 拆句前半
	}
	zh := []string{
		"它常和友達、会議、デート搭配，也可以用約束があります。",
		"先拆：今月是时间；外食が主语，多かった是过去形。",
		"意思是“和朋友约好了”。",
		"这个词在便利店结账时一定会听到。",
		"日常问熟人可以用「誰ですか」。", // 中文句引用日语，2026-11-04 线上误判用例
	}
	for _, s := range ja {
		if !isJaLine(s) {
			t.Errorf("应判为日语行：%s", s)
		}
	}
	for _, s := range zh {
		if isJaLine(s) {
			t.Errorf("应判为中文行：%s", s)
		}
	}
}

func TestRepairInsertsMissingCue(t *testing.T) {
	// 两句"说完"的日语连排：第二句前应自动补一行中文引子，而不是整段重试
	in := []Line{
		{Role: RoleA, Text: "山田さんは先生です。"},
		{Role: RoleB, Text: "誰ですか。"},
	}
	out := repair(in)
	if err := checkLines(out); err != nil {
		t.Fatalf("补引子后应通过校验：%v", err)
	}
	if len(out) != 3 || out[1].Text != "听——" || out[2].Text != "誰ですか。" {
		t.Errorf("日语行前应补中文引子，得到：%v", out)
	}
	// 拆句后半（以、结尾）不该补引子
	cont := repair([]Line{
		{Role: RoleA, Text: "山田さんは、"},
		{Role: RoleB, Text: "先生です。"},
	})
	if len(cont) != 2 {
		t.Errorf("拆句后半不应补引子，得到 %d 行", len(cont))
	}
}

func TestRepairSplitsLongLines(t *testing.T) {
	long := "这个词在便利店结账的时候一定会听到，我们先听一遍原声，然后一个成分一个成分地拆开来讲清楚。"
	lines := repair([]Line{{Role: RoleA, Text: long}})
	if len(lines) < 2 {
		t.Fatalf("超长中文行应被拆开，得到 %d 行", len(lines))
	}
	for _, l := range lines {
		if len([]rune(l.Text)) > maxZhLineRunes {
			t.Errorf("拆完仍超长：%s", l.Text)
		}
	}
	// 拆出的连排片段若到第 3 行，应翻给另一位主持（两人都双语），整体仍要合规
	if err := checkLines(lines); err != nil {
		t.Errorf("拆行后应通过行级校验：%v", err)
	}
	joined := ""
	for _, l := range lines {
		joined += l.Text
	}
	if joined != long {
		t.Errorf("拆行不应丢字：\n%s\n%s", joined, long)
	}
}

func TestRepairFixesRoleRun(t *testing.T) {
	in := []Line{{Role: RoleA, Text: "一"}, {Role: RoleA, Text: "二"}, {Role: RoleA, Text: "三"}, {Role: RoleA, Text: "四"}}
	out := repair(in)
	if err := checkLines(out); err != nil {
		t.Errorf("修复后仍不合规：%v", err)
	}
}

func TestFillSayMixesLanguages(t *testing.T) {
	words := []WordBrief{{Surface: "中国人", Reading: "ちゅうごくじん"}, {Surface: "山田", Reading: "やまだ"}}
	lines := []Line{
		{Role: RoleA, Text: "小李是中国人。介绍别人国籍的说法。"},   // 行中混读：想要
		{Role: RoleB, Text: "李さんは中国人ですか。"},             // 纯日语行：整行假名化
		{Role: RoleA, Text: "山田这个词，放在句首很常见。"},        // 行首是词 → 合成后行首假名
	}
	FillSay(lines, words)
	if lines[0].Say != "小李是ちゅうごくじん。介绍别人国籍的说法。" {
		t.Errorf("中文行应只把日语词换假名：%s", lines[0].Say)
	}
	if lines[1].Say != "李さんはちゅうごくじんですか。" {
		t.Errorf("日语行应整行假名化：%s", lines[1].Say)
	}
	if startsKana(lines[2].Say) || !strings.HasPrefix(lines[2].Say, "这个词——") {
		t.Errorf("行首是日语词的中文行应自动垫中文引子：%s", lines[2].Say)
	}
	if startsKana(lines[0].Say) {
		t.Error("行中混读不应被判为假名开头")
	}
}
