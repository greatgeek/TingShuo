// Package review 实现复习调度（需求文档 §8.5，简化间隔重复，纯函数可单测）。
//
// 规则：初始间隔 1 天；答对 → 间隔 ×2（封顶 30 天）；答错 → 重置为 1 天。
// 不引入 SM-2 式难度系数——答错本身就是最强的难度信号（间隔效应+测试效应，
// 不采信网络流传的艾宾浩斯时间表，见 FR-7 注）。
//
// 已知文档差异：AC-7.1 表述首次到期为 D+2，而 §8.5 代码注释为「首次入池
// interval=1（即 D+1）」。本实现按 §8.5 代码为准，M3 复习页集成测试时
// 按实际体验裁决口径。
package review

import "time"

// Result 是一次复习的自评结果。
type Result string

const (
	Pass Result = "pass"
	Fail Result = "fail"
)

const (
	// FailResetDays 答错后重置为 1 天。
	FailResetDays = 1
	// MinIntervalDays 答对后的最短间隔（×2 后至少 2 天）。
	MinIntervalDays = 2
	// MaxIntervalDays 间隔封顶 30 天。
	MaxIntervalDays = 30
)

// NextInterval 计算下一次复习间隔（§8.5 服务端纯函数）。
//
// 首次入池 interval=1；答对 cur×2 封顶 30、下限 2；答错一律重置 1。
func NextInterval(curDays int, result Result) int {
	if result == Fail {
		return FailResetDays
	}
	next := curDays * 2
	if next > MaxIntervalDays {
		next = MaxIntervalDays
	}
	if next < MinIntervalDays {
		next = MinIntervalDays
	}
	return next
}

// ApplyResult 对复习项应用一次结果，返回新间隔、新到期日与 error_count 增量。
// base 为结果发生日（通常为今天）；due_date = base + interval。
func ApplyResult(curDays int, result Result, base time.Time) (interval int, dueDate time.Time, errCountDelta int) {
	interval = NextInterval(curDays, result)
	if result == Fail {
		errCountDelta = 1
	}
	dueDate = base.AddDate(0, 0, interval)
	return interval, dueDate, errCountDelta
}
