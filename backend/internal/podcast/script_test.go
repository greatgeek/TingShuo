package podcast

import "testing"

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
	}
	zh := []string{
		"它常和友達、会議、デート搭配，也可以用約束があります。",
		"先拆：今月是时间；外食が主语，多かった是过去形。",
		"意思是“和朋友约好了”。",
		"这个词在便利店结账时一定会听到。",
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
