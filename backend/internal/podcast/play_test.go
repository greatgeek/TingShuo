package podcast

import "testing"

func TestParseRange(t *testing.T) {
	const size = int64(1000)
	cases := []struct {
		hdr        string
		start, end int64
		ranged     bool
	}{
		{"", 0, 999, false},
		{"bytes=0-99", 0, 99, true},
		{"bytes=100-", 100, 999, true},
		{"bytes=-50", 950, 999, true},
		{"bytes=0-2000", 0, 999, true}, // end 越界夹到末尾
		{"bytes=1000-", 0, 999, false}, // start 越界视为整对象
		{"bytes=abc", 0, 999, false},
		{"items=0-9", 0, 999, false},
	}
	for _, c := range cases {
		s, e, r := parseRange(c.hdr, size)
		if s != c.start || e != c.end || r != c.ranged {
			t.Errorf("parseRange(%q) = %d,%d,%v，想要 %d,%d,%v", c.hdr, s, e, r, c.start, c.end, c.ranged)
		}
	}
}

func TestPlayTicket(t *testing.T) {
	t.Setenv("JWT_SECRET", "test-secret")
	tk := playTicket("ep1", "user1")
	if uid, ok := verifyPlayTicket(tk, "ep1"); !ok || uid != "user1" {
		t.Fatalf("票据应能验回本人，得到 %q %v", uid, ok)
	}
	if _, ok := verifyPlayTicket(tk, "ep2"); ok {
		t.Error("票据不应跨集通用")
	}
	if _, ok := verifyPlayTicket(tk+"x", "ep1"); ok {
		t.Error("被篡改的票据不应通过")
	}
	if _, ok := verifyPlayTicket("not-a-ticket", "ep1"); ok {
		t.Error("垃圾票据不应通过")
	}
}
