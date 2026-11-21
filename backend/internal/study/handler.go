// Package study 学习足迹：前台有效学习时长、学习日历、连续天数、每日目标与累计里程碑。
// 计分以服务端为准：客户端只在「学习屏 + 页面可见 + 窗口聚焦 + 有交互或媒体在播」时发心跳，
// 服务端按距上次计分的间隔累加（多设备/多标签并集去重），并做单日总量与纯听力上限。
package study

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/greatgeek/TingShuo/backend/internal/auth"
	"github.com/greatgeek/TingShuo/backend/internal/httpx"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// 东八区无夏令时，用固定时区避免依赖系统 tzdata。
var cst = time.FixedZone("CST", 8*60*60)

const (
	firstCreditSeconds = 20 // 当天第一次心跳
	maxCreditSeconds   = 25 // 单次心跳上限：容忍定时器抖动，不给长时间离开补记
	maxDaySeconds      = 8 * 3600
	maxListenSeconds   = 3 * 3600
	defaultGoalMin     = 15
)

var featureSet = map[string]bool{
	"learn": true, "review": true, "library": true, "wordstudy": true,
	"aiask": true, "notebook": true, "grammar": true, "quiz": true,
	"recite": true, "recite-play": true, "nbres": true,
}

var hourMilestones = []int64{10, 50, 100, 200, 500, 1000}
var streakMilestones = []int{7, 30, 100, 365}

type Handler struct {
	pool *pgxpool.Pool
	now  func() time.Time
}

func NewHandler(pool *pgxpool.Pool) *Handler { return &Handler{pool: pool, now: time.Now} }

type querier interface {
	Query(context.Context, string, ...any) (pgx.Rows, error)
	QueryRow(context.Context, string, ...any) pgx.Row
	Exec(context.Context, string, ...any) (pgconn.CommandTag, error)
}

// creditSeconds 计算本次心跳可计秒数；last 为 nil 表示当天首跳。
func creditSeconds(last *time.Time, now time.Time, active, listen int, mode string) int {
	credit := firstCreditSeconds
	if last != nil {
		elapsed := int(now.Sub(*last) / time.Second)
		switch {
		case elapsed <= 0:
			credit = 0
		case elapsed < maxCreditSeconds:
			credit = elapsed
		default:
			credit = maxCreditSeconds
		}
	}
	if remain := maxDaySeconds - (active + listen); credit > remain {
		credit = remain
	}
	if mode == "listen" {
		if remain := maxListenSeconds - listen; credit > remain {
			credit = remain
		}
	}
	if credit < 0 {
		return 0
	}
	return credit
}

func splitCredit(credit int, mode string) (active, listen int) {
	if credit <= 0 {
		return 0, 0
	}
	if mode == "listen" {
		return 0, credit
	}
	return credit, 0
}

type pingReq struct {
	Feature string `json:"feature"`
	Mode    string `json:"mode"`
}

type pingResp struct {
	Credited      int      `json:"credited"`
	Day           string   `json:"day"`
	ActiveSeconds int      `json:"active_seconds"`
	ListenSeconds int      `json:"listen_seconds"`
	TotalSeconds  int      `json:"total_seconds"`
	TotalAll      int64    `json:"total_all_seconds"`
	GoalMin       int      `json:"goal_min"`
	Streak        int      `json:"streak"`
	NewMilestones []string `json:"new_milestones"`
}

// Ping POST /study/ping：学习屏每 20 秒一次心跳，mode=active|listen。
func (h *Handler) Ping(w http.ResponseWriter, r *http.Request) {
	userID := auth.FromContext(r.Context())
	var req pingReq
	if e := httpx.Decode(r, &req); e != nil {
		httpx.Err(w, e)
		return
	}
	if !featureSet[req.Feature] {
		httpx.Err(w, httpx.BadRequest("FEATURE_INVALID", "未知的学习模块"))
		return
	}
	mode := "active"
	if req.Mode == "listen" {
		mode = "listen"
	}

	ctx := r.Context()
	now := h.now().In(cst)
	day := now.Format("2006-01-02")

	tx, err := h.pool.Begin(ctx)
	if err != nil {
		httpx.Err(w, httpx.Internal(err.Error()))
		return
	}
	defer tx.Rollback(ctx)

	var active, listen int
	var last time.Time
	err = tx.QueryRow(ctx, `
		SELECT active_seconds, listen_seconds, last_credit_at
		FROM study_day WHERE user_id = $1 AND day = $2::date FOR UPDATE`, userID, day).
		Scan(&active, &listen, &last)

	credit := 0
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		credit = creditSeconds(nil, now, 0, 0, mode)
		addActive, addListen := splitCredit(credit, mode)
		active, listen = addActive, addListen
		_, err = tx.Exec(ctx, `
			INSERT INTO study_day (user_id, day, active_seconds, listen_seconds, last_credit_at)
			VALUES ($1, $2::date, $3, $4, $5)`, userID, day, active, listen, now)
	case err != nil:
		httpx.Err(w, httpx.Internal(err.Error()))
		return
	default:
		credit = creditSeconds(&last, now, active, listen, mode)
		addActive, addListen := splitCredit(credit, mode)
		active += addActive
		listen += addListen
		_, err = tx.Exec(ctx, `
			UPDATE study_day SET active_seconds = active_seconds + $3, listen_seconds = listen_seconds + $4,
			       last_credit_at = $5
			WHERE user_id = $1 AND day = $2::date`, userID, day, addActive, addListen, now)
	}
	if err != nil {
		httpx.Err(w, httpx.Internal(err.Error()))
		return
	}

	if credit > 0 {
		if _, err = tx.Exec(ctx, `
			INSERT INTO study_feature_day (user_id, day, feature, seconds)
			VALUES ($1, $2::date, $3, $4)
			ON CONFLICT (user_id, day, feature) DO UPDATE SET seconds = study_feature_day.seconds + EXCLUDED.seconds`,
			userID, day, req.Feature, credit); err != nil {
			httpx.Err(w, httpx.Internal(err.Error()))
			return
		}
	}

	days, first, err := loadDays(ctx, tx, userID, day)
	if err != nil {
		httpx.Err(w, httpx.Internal(err.Error()))
		return
	}
	streak := computeStreak(days, first, day)
	totalActive, totalListen, err := loadTotal(ctx, tx, userID)
	if err != nil {
		httpx.Err(w, httpx.Internal(err.Error()))
		return
	}
	newMilestones, err := awardMilestones(ctx, tx, userID, totalActive+totalListen, streak.Current)
	if err != nil {
		httpx.Err(w, httpx.Internal(err.Error()))
		return
	}
	goal, err := goalOf(ctx, tx, userID)
	if err != nil {
		httpx.Err(w, httpx.Internal(err.Error()))
		return
	}
	if err := tx.Commit(ctx); err != nil {
		httpx.Err(w, httpx.Internal(err.Error()))
		return
	}
	if newMilestones == nil {
		newMilestones = []string{}
	}

	httpx.WriteJSON(w, http.StatusOK, pingResp{
		Credited:      credit,
		Day:           day,
		ActiveSeconds: active,
		ListenSeconds: listen,
		TotalSeconds:  active + listen,
		TotalAll:      totalActive + totalListen,
		GoalMin:       goal,
		Streak:        streak.Current,
		NewMilestones: newMilestones,
	})
}

type calendarDay struct {
	Day    string `json:"day"`
	Active int    `json:"active_seconds"`
	Listen int    `json:"listen_seconds"`
	Total  int    `json:"total_seconds"`
}

type milestoneItem struct {
	Key        string    `json:"key"`
	AchievedAt time.Time `json:"achieved_at"`
}

// Overview GET /study/overview：今日/累计/连续/本周/目标/里程碑一次取齐。
func (h *Handler) Overview(w http.ResponseWriter, r *http.Request) {
	userID := auth.FromContext(r.Context())
	ctx := r.Context()
	now := h.now().In(cst)
	today := now.Format("2006-01-02")

	goal, err := goalOf(ctx, h.pool, userID)
	if err != nil {
		httpx.Err(w, httpx.Internal(err.Error()))
		return
	}

	var todayActive, todayListen int
	if err := h.pool.QueryRow(ctx, `
		SELECT active_seconds, listen_seconds FROM study_day
		WHERE user_id = $1 AND day = $2::date`, userID, today).
		Scan(&todayActive, &todayListen); err != nil && !errors.Is(err, pgx.ErrNoRows) {
		httpx.Err(w, httpx.Internal(err.Error()))
		return
	}

	totalActive, totalListen, err := loadTotal(ctx, h.pool, userID)
	if err != nil {
		httpx.Err(w, httpx.Internal(err.Error()))
		return
	}

	offset := (int(now.Weekday()) + 6) % 7 // 周一为一周起点
	weekStart := now.AddDate(0, 0, -offset)
	week := make([]calendarDay, 7)
	weekIndex := map[string]int{}
	for i := 0; i < 7; i++ {
		d := weekStart.AddDate(0, 0, i).Format("2006-01-02")
		week[i] = calendarDay{Day: d}
		weekIndex[d] = i
	}
	weekEnd := weekStart.AddDate(0, 0, 6).Format("2006-01-02")
	rows, err := h.pool.Query(ctx, `
		SELECT to_char(day, 'YYYY-MM-DD'), active_seconds, listen_seconds
		FROM study_day WHERE user_id = $1 AND day BETWEEN $2::date AND $3::date
		ORDER BY day`, userID, weekStart.Format("2006-01-02"), weekEnd)
	if err != nil {
		httpx.Err(w, httpx.Internal(err.Error()))
		return
	}
	weekActive, weekListen := 0, 0
	for rows.Next() {
		var d string
		var a, l int
		if err := rows.Scan(&d, &a, &l); err != nil {
			rows.Close()
			httpx.Err(w, httpx.Internal(err.Error()))
			return
		}
		if i, ok := weekIndex[d]; ok {
			week[i].Active, week[i].Listen, week[i].Total = a, l, a+l
		}
		weekActive += a
		weekListen += l
	}
	rows.Close()

	days, first, err := loadDays(ctx, h.pool, userID, today)
	if err != nil {
		httpx.Err(w, httpx.Internal(err.Error()))
		return
	}
	streak := computeStreak(days, first, today)

	milestones := []milestoneItem{}
	msRows, err := h.pool.Query(ctx,
		`SELECT key, achieved_at FROM study_milestone WHERE user_id = $1 ORDER BY achieved_at`, userID)
	if err != nil {
		httpx.Err(w, httpx.Internal(err.Error()))
		return
	}
	for msRows.Next() {
		var it milestoneItem
		if err := msRows.Scan(&it.Key, &it.AchievedAt); err != nil {
			msRows.Close()
			httpx.Err(w, httpx.Internal(err.Error()))
			return
		}
		milestones = append(milestones, it)
	}
	msRows.Close()

	todayTotal := todayActive + todayListen
	httpx.WriteJSON(w, http.StatusOK, map[string]any{
		"today":         today,
		"goal_min":      goal,
		"goal_seconds":  goal * 60,
		"today_active":  todayActive,
		"today_listen":  todayListen,
		"today_total":   todayTotal,
		"today_reached": todayTotal >= goal*60,
		"total_active":  totalActive,
		"total_listen":  totalListen,
		"total_seconds": totalActive + totalListen,
		"streak":        streak.Current,
		"best_streak":   streak.Best,
		"grace_used":    streak.GraceUsed,
		"week_start":    weekStart.Format("2006-01-02"),
		"week":          week,
		"week_active":   weekActive,
		"week_listen":   weekListen,
		"milestones":    milestones,
		"next_hours":    nextHoursMilestone(totalActive + totalListen),
		"next_streak":   nextStreakMilestone(streak.Current),
	})
}

// Calendar GET /study/calendar?from=&to=：区间内每天时长（只返回有记录的天），默认本月。
func (h *Handler) Calendar(w http.ResponseWriter, r *http.Request) {
	userID := auth.FromContext(r.Context())
	now := h.now().In(cst)
	from := strings.TrimSpace(r.URL.Query().Get("from"))
	to := strings.TrimSpace(r.URL.Query().Get("to"))
	if from == "" || to == "" {
		first := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, cst)
		from = first.Format("2006-01-02")
		to = first.AddDate(0, 1, -1).Format("2006-01-02")
	}
	t1, err1 := time.Parse("2006-01-02", from)
	t2, err2 := time.Parse("2006-01-02", to)
	if err1 != nil || err2 != nil || t1.After(t2) {
		httpx.Err(w, httpx.BadRequest("RANGE_INVALID", "日期范围无效"))
		return
	}
	span := int(t2.Sub(t1).Hours()/24) + 1
	if span > 400 {
		httpx.Err(w, httpx.BadRequest("RANGE_TOO_LARGE", "查询范围最多 400 天"))
		return
	}

	rows, err := h.pool.Query(r.Context(), `
		SELECT to_char(day, 'YYYY-MM-DD'), active_seconds, listen_seconds
		FROM study_day WHERE user_id = $1 AND day BETWEEN $2::date AND $3::date
		ORDER BY day`, userID, from, to)
	if err != nil {
		httpx.Err(w, httpx.Internal(err.Error()))
		return
	}
	list := []calendarDay{}
	totalActive, totalListen := 0, 0
	for rows.Next() {
		var d calendarDay
		if err := rows.Scan(&d.Day, &d.Active, &d.Listen); err != nil {
			rows.Close()
			httpx.Err(w, httpx.Internal(err.Error()))
			return
		}
		d.Total = d.Active + d.Listen
		totalActive += d.Active
		totalListen += d.Listen
		list = append(list, d)
	}
	rows.Close()

	httpx.WriteJSON(w, http.StatusOK, map[string]any{
		"from":          from,
		"to":            to,
		"days":          list,
		"active_days":   len(list),
		"total_active":  totalActive,
		"total_listen":  totalListen,
		"total_seconds": totalActive + totalListen,
	})
}

type featureItem struct {
	Feature string `json:"feature"`
	Seconds int    `json:"seconds"`
}

// Day GET /study/day/{date}：单日主动/听力拆分与各功能用时。
func (h *Handler) Day(w http.ResponseWriter, r *http.Request) {
	userID := auth.FromContext(r.Context())
	date := strings.TrimSpace(chi.URLParam(r, "date"))
	if _, err := time.Parse("2006-01-02", date); err != nil {
		httpx.Err(w, httpx.BadRequest("DATE_INVALID", "日期格式应为 YYYY-MM-DD"))
		return
	}
	ctx := r.Context()

	goal, err := goalOf(ctx, h.pool, userID)
	if err != nil {
		httpx.Err(w, httpx.Internal(err.Error()))
		return
	}

	var active, listen int
	if err := h.pool.QueryRow(ctx, `
		SELECT active_seconds, listen_seconds FROM study_day
		WHERE user_id = $1 AND day = $2::date`, userID, date).Scan(&active, &listen); err != nil && !errors.Is(err, pgx.ErrNoRows) {
		httpx.Err(w, httpx.Internal(err.Error()))
		return
	}

	features := []featureItem{}
	rows, err := h.pool.Query(ctx, `
		SELECT feature, seconds FROM study_feature_day
		WHERE user_id = $1 AND day = $2::date AND seconds > 0
		ORDER BY seconds DESC, feature`, userID, date)
	if err != nil {
		httpx.Err(w, httpx.Internal(err.Error()))
		return
	}
	for rows.Next() {
		var it featureItem
		if err := rows.Scan(&it.Feature, &it.Seconds); err != nil {
			rows.Close()
			httpx.Err(w, httpx.Internal(err.Error()))
			return
		}
		features = append(features, it)
	}
	rows.Close()

	httpx.WriteJSON(w, http.StatusOK, map[string]any{
		"day":            date,
		"active_seconds": active,
		"listen_seconds": listen,
		"total_seconds":  active + listen,
		"qualified":      active+listen >= qualifiedSeconds,
		"goal_min":       goal,
		"goal_reached":   active+listen >= goal*60,
		"features":       features,
	})
}

type settingReq struct {
	DailyGoalMin int `json:"daily_goal_min"`
}

// GetSettings GET /study/settings
func (h *Handler) GetSettings(w http.ResponseWriter, r *http.Request) {
	goal, err := goalOf(r.Context(), h.pool, auth.FromContext(r.Context()))
	if err != nil {
		httpx.Err(w, httpx.Internal(err.Error()))
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"daily_goal_min": goal})
}

// PutSettings PUT /study/settings：每日目标分钟数（5–240）。
func (h *Handler) PutSettings(w http.ResponseWriter, r *http.Request) {
	userID := auth.FromContext(r.Context())
	var req settingReq
	if e := httpx.Decode(r, &req); e != nil {
		httpx.Err(w, e)
		return
	}
	if req.DailyGoalMin < 5 || req.DailyGoalMin > 240 {
		httpx.Err(w, httpx.BadRequest("GOAL_INVALID", "每日目标需在 5–240 分钟之间"))
		return
	}
	if _, err := h.pool.Exec(r.Context(), `
		INSERT INTO study_setting (user_id, daily_goal_min, updated_at)
		VALUES ($1, $2, now())
		ON CONFLICT (user_id) DO UPDATE SET daily_goal_min = EXCLUDED.daily_goal_min, updated_at = now()`,
		userID, req.DailyGoalMin); err != nil {
		httpx.Err(w, httpx.Internal(err.Error()))
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"daily_goal_min": req.DailyGoalMin})
}

func goalOf(ctx context.Context, q querier, userID string) (int, error) {
	var goal int
	if err := q.QueryRow(ctx,
		`SELECT daily_goal_min FROM study_setting WHERE user_id = $1`, userID).Scan(&goal); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return defaultGoalMin, nil
		}
		return 0, err
	}
	return goal, nil
}

func loadTotal(ctx context.Context, q querier, userID string) (active, listen int64, err error) {
	err = q.QueryRow(ctx, `
		SELECT COALESCE(sum(active_seconds), 0), COALESCE(sum(listen_seconds), 0)
		FROM study_day WHERE user_id = $1`, userID).Scan(&active, &listen)
	return active, listen, err
}

func loadDays(ctx context.Context, q querier, userID, until string) (map[string]int, string, error) {
	rows, err := q.Query(ctx, `
		SELECT to_char(day, 'YYYY-MM-DD'), active_seconds + listen_seconds
		FROM study_day WHERE user_id = $1 AND day <= $2::date
		ORDER BY day`, userID, until)
	if err != nil {
		return nil, "", err
	}
	defer rows.Close()
	days := map[string]int{}
	first := ""
	for rows.Next() {
		var d string
		var s int
		if err := rows.Scan(&d, &s); err != nil {
			return nil, "", err
		}
		if first == "" {
			first = d
		}
		days[d] = s
	}
	return days, first, rows.Err()
}

func awardMilestones(ctx context.Context, q querier, userID string, totalSeconds int64, streak int) ([]string, error) {
	var keys []string
	for _, hours := range hourMilestones {
		if totalSeconds < hours*3600 {
			continue
		}
		key := "hours_" + strconv.FormatInt(hours, 10)
		added, err := insertMilestone(ctx, q, userID, key)
		if err != nil {
			return nil, err
		}
		if added {
			keys = append(keys, key)
		}
	}
	for _, days := range streakMilestones {
		if streak < days {
			continue
		}
		key := "streak_" + strconv.Itoa(days)
		added, err := insertMilestone(ctx, q, userID, key)
		if err != nil {
			return nil, err
		}
		if added {
			keys = append(keys, key)
		}
	}
	return keys, nil
}

func insertMilestone(ctx context.Context, q querier, userID, key string) (bool, error) {
	tag, err := q.Exec(ctx,
		`INSERT INTO study_milestone (user_id, key) VALUES ($1, $2) ON CONFLICT DO NOTHING`, userID, key)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() == 1, nil
}

func nextHoursMilestone(total int64) any {
	for _, hours := range hourMilestones {
		if total < hours*3600 {
			return map[string]any{
				"key":            "hours_" + strconv.FormatInt(hours, 10),
				"target_seconds": hours * 3600,
				"remain_seconds": hours*3600 - total,
			}
		}
	}
	return nil
}

func nextStreakMilestone(current int) any {
	for _, days := range streakMilestones {
		if current < days {
			return map[string]any{
				"key":         "streak_" + strconv.Itoa(days),
				"target_days": days,
				"remain_days": days - current,
			}
		}
	}
	return nil
}
