package study

import (
	"testing"
	"time"
)

func dayTime(s string) time.Time {
	t, _ := time.Parse("2006-01-02", s)
	return t
}

func TestCreditSeconds(t *testing.T) {
	now := dayTime("2026-11-22")
	cases := []struct {
		name           string
		last           *time.Time
		active, listen int
		mode           string
		want           int
	}{
		{"当天首跳", nil, 0, 0, "active", 20},
		{"正常心跳", ptr(now.Add(-20 * time.Second)), 120, 0, "active", 20},
		{"定时器抖动", ptr(now.Add(-22 * time.Second)), 120, 0, "active", 22},
		{"长时间离开封顶", ptr(now.Add(-10 * time.Minute)), 120, 0, "active", 25},
		{"时钟回拨", ptr(now.Add(5 * time.Second)), 120, 0, "active", 0},
		{"总时长封顶", ptr(now.Add(-20 * time.Second)), maxDaySeconds - 5, 0, "active", 5},
		{"听力封顶", ptr(now.Add(-20 * time.Second)), 0, maxListenSeconds - 5, "listen", 5},
		{"听力但总时长更紧", ptr(now.Add(-20 * time.Second)), maxDaySeconds - 5, 0, "listen", 5},
		{"总量已满", ptr(now.Add(-20 * time.Second)), maxDaySeconds, 0, "active", 0},
	}
	for _, c := range cases {
		if got := creditSeconds(c.last, now, c.active, c.listen, c.mode); got != c.want {
			t.Errorf("%s: got %d want %d", c.name, got, c.want)
		}
	}
}

func ptr(t time.Time) *time.Time { return &t }

func TestComputeStreak(t *testing.T) {
	q := qualifiedSeconds + 60 // 合格日
	s := 60                    // 不足 5 分钟

	cases := []struct {
		name      string
		days      map[string]int
		first     string
		today     string
		wantCur   int
		wantBest  int
		wantGrace bool
	}{
		{
			name:  "今天首次达标",
			days:  map[string]int{"2026-11-22": q},
			first: "2026-11-22", today: "2026-11-22",
			wantCur: 1, wantBest: 1,
		},
		{
			name:  "今天未达标不打断",
			days:  map[string]int{"2026-11-20": q, "2026-11-21": q, "2026-11-22": s},
			first: "2026-11-20", today: "2026-11-22",
			wantCur: 2, wantBest: 2,
		},
		{
			name:  "单日休息日保住连续",
			days:  map[string]int{"2026-11-18": q, "2026-11-19": q, "2026-11-21": q, "2026-11-22": q},
			first: "2026-11-18", today: "2026-11-22",
			wantCur: 4, wantBest: 4, wantGrace: true,
		},
		{
			name:  "七天内第二次断档打断后重新起算",
			days:  map[string]int{"2026-11-18": q, "2026-11-19": q, "2026-11-22": q},
			first: "2026-11-18", today: "2026-11-22",
			wantCur: 1, wantBest: 2,
		},
		{
			name: "历史最佳跨过断档",
			days: map[string]int{
				"2026-11-01": q, "2026-11-02": q, "2026-11-03": q, "2026-11-04": q, "2026-11-05": q,
				"2026-11-11": q, "2026-11-12": q, "2026-11-13": q,
			},
			first: "2026-11-01", today: "2026-11-13",
			wantCur: 3, wantBest: 5,
		},
		{
			name:  "分钟不足的日子不算",
			days:  map[string]int{"2026-11-20": q, "2026-11-21": s, "2026-11-22": s},
			first: "2026-11-20", today: "2026-11-22",
			wantCur: 1, wantBest: 1, wantGrace: true,
		},
		{
			name:  "无记录",
			days:  map[string]int{},
			first: "", today: "2026-11-22",
			wantCur: 0, wantBest: 0,
		},
	}
	for _, c := range cases {
		got := computeStreak(c.days, c.first, c.today)
		if got.Current != c.wantCur || got.Best != c.wantBest || got.GraceUsed != c.wantGrace {
			t.Errorf("%s: got %+v want cur=%d best=%d grace=%v", c.name, got, c.wantCur, c.wantBest, c.wantGrace)
		}
	}
}
