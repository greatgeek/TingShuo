package review

import (
	"testing"
	"time"
)

func TestNextInterval(t *testing.T) {
	cases := []struct {
		cur    int
		result Result
		want   int
	}{
		// 首次入池 interval=1，答对 → 2
		{1, Pass, 2},
		{2, Pass, 4},
		{4, Pass, 8},
		{8, Pass, 16},
		{16, Pass, 30}, // 32 封顶 30
		{30, Pass, 30}, // 已在封顶，保持 30
		{25, Pass, 30}, // 50 封顶 30
		// 任何间隔下答错一律重置 1
		{1, Fail, 1},
		{8, Fail, 1},
		{30, Fail, 1},
		// 异常入参防御：0/负数答对下限抬到 2
		{0, Pass, 2},
		{-5, Pass, 2},
	}
	for _, c := range cases {
		if got := NextInterval(c.cur, c.result); got != c.want {
			t.Errorf("NextInterval(%d, %s) = %d, 期望 %d", c.cur, c.result, got, c.want)
		}
	}
}

func TestApplyResult(t *testing.T) {
	base := time.Date(2026, 10, 26, 0, 0, 0, 0, time.UTC)

	interval, due, errDelta := ApplyResult(1, Pass, base)
	if interval != 2 || !due.Equal(base.AddDate(0, 0, 2)) || errDelta != 0 {
		t.Errorf("ApplyResult(1, Pass) = (%d, %v, %d), 期望 (2, D+2, 0)", interval, due, errDelta)
	}

	interval, due, errDelta = ApplyResult(4, Fail, base)
	if interval != 1 || !due.Equal(base.AddDate(0, 0, 1)) || errDelta != 1 {
		t.Errorf("ApplyResult(4, Fail) = (%d, %v, %d), 期望 (1, D+1, 1)", interval, due, errDelta)
	}
}
