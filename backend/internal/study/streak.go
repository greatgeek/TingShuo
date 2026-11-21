package study

import "time"

// 一个「学习日」的门槛：当日累计满 5 分钟才计入连续天数。
const qualifiedSeconds = 300

type streakResult struct {
	Current   int
	Best      int
	GraceUsed bool
}

func dayDiff(from, to string) int {
	a, err1 := time.Parse("2006-01-02", from)
	b, err2 := time.Parse("2006-01-02", to)
	if err1 != nil || err2 != nil {
		return 0
	}
	return int(b.Sub(a).Hours() / 24)
}

// computeStreak 从首个有记录的日子顺序扫到今天：
// - 当日 >= 5 分钟记 1 天；
// - 断档日每 7 天窗口内允许 1 天作休息日（连续天数不中断也不增加）；
// - 今天还没结束，未满 5 分钟不算断。
func computeStreak(seconds map[string]int, first, today string) streakResult {
	var res streakResult
	if first == "" || today == "" {
		return res
	}
	start, err1 := time.Parse("2006-01-02", first)
	end, err2 := time.Parse("2006-01-02", today)
	if err1 != nil || err2 != nil || start.After(end) {
		return res
	}
	if end.Sub(start) > 4000*24*time.Hour {
		start = end.AddDate(0, 0, -4000)
	}

	run := 0
	active := false
	lastGrace := ""
	for d := start; !d.After(end); d = d.AddDate(0, 0, 1) {
		ds := d.Format("2006-01-02")
		if seconds[ds] >= qualifiedSeconds {
			if active {
				run++
			} else {
				run = 1
				active = true
				lastGrace = ""
			}
			if run > res.Best {
				res.Best = run
			}
			continue
		}
		if ds == today || !active {
			continue
		}
		if lastGrace == "" || dayDiff(lastGrace, ds) >= 7 {
			lastGrace = ds
			continue
		}
		active = false
		run = 0
		lastGrace = ""
	}
	if active {
		res.Current = run
		res.GraceUsed = lastGrace != ""
	}
	return res
}
