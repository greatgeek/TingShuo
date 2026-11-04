package podcast

import "testing"

func TestKanaize(t *testing.T) {
	words := []WordBrief{
		{Surface: "食費", Reading: "しょくひ"},
		{Surface: "外食", Reading: "がいしょく"},
		{Surface: "食", Reading: "しょく"},
		{Surface: "多い", Reading: ""},
		{Surface: "財布", Reading: "さいふ"},
	}
	cases := []struct{ in, want string }{
		{"这个月外食太多，所以伙食费涨了。", "这个月がいしょく太多，所以伙食费涨了。"},
		{"今月、外食が多い。なので、食費が増えました。", "今月、がいしょくが多い。なので、しょくひが増えました。"},
		{"没有生词的中文行保持原样。", "没有生词的中文行保持原样。"},
	}
	for _, c := range cases {
		if got := Kanaize(c.in, words); got != c.want {
			t.Errorf("Kanaize(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestSessionLinesUsesSay(t *testing.T) {
	got, err := SessionLines([]Line{
		{Role: RoleA, Text: "食費就是伙食费", Say: "しょくひ就是伙食费"},
		{Role: RoleB, Text: "食費。しょくひ、です。"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if got[0].Text != "しょくひ就是伙食费" {
		t.Errorf("有 Say 时应送 Say，得到 %q", got[0].Text)
	}
	if got[1].Text != "食費。しょくひ、です。" {
		t.Errorf("无 Say 时应回退 Text，得到 %q", got[1].Text)
	}
}
