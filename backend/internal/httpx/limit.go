// 限流与配额：令牌桶（按 IP/用户）+ 每日配额（按用户），防暴力与成本滥用。
package httpx

import (
	"net/http"
	"sync"
	"time"
)

// Limiter 令牌桶：rate 为每分钟补充数，burst 为桶容量。
type Limiter struct {
	mu    sync.Mutex
	m     map[string]*bucket
	rate  float64 // tokens / min
	burst float64
}

type bucket struct {
	tok  float64
	last time.Time
}

func NewLimiter(ratePerMin, burst float64) *Limiter {
	return &Limiter{m: map[string]*bucket{}, rate: ratePerMin, burst: burst}
}

func (l *Limiter) Allow(key string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	b, ok := l.m[key]
	if !ok {
		b = &bucket{tok: l.burst, last: time.Now()}
		l.m[key] = b
	}
	now := time.Now()
	b.tok += now.Sub(b.last).Minutes() * l.rate
	if b.tok > l.burst {
		b.tok = l.burst
	}
	b.last = now
	if b.tok < 1 {
		return false
	}
	b.tok--
	return true
}

// 定期清桶防内存增长（惰性：超 1 小时未访问即删）
func (l *Limiter) sweep() {
	l.mu.Lock()
	defer l.mu.Unlock()
	for k, b := range l.m {
		if time.Since(b.last) > time.Hour {
			delete(l.m, k)
		}
	}
}

// DailyQuota 每 key 每日计数上限（自然日，进程内）。
type DailyQuota struct {
	mu  sync.Mutex
	m   map[string]dayCount
	cap int
}

type dayCount struct {
	day string
	n   int
}

func NewDailyQuota(cap int) *DailyQuota {
	return &DailyQuota{m: map[string]dayCount{}, cap: cap}
}

func (q *DailyQuota) Allow(key string) bool {
	today := time.Now().Format("2006-01-02")
	q.mu.Lock()
	defer q.mu.Unlock()
	c := q.m[key]
	if c.day != today {
		c = dayCount{day: today}
	}
	if c.n >= q.cap {
		q.m[key] = c
		return false
	}
	c.n++
	q.m[key] = c
	return true
}

// Limit 中间件：不放过则 429。
func Limit(l *Limiter, keyFn func(r *http.Request) string) func(http.Handler) http.Handler {
	go func() {
		for range time.Tick(10 * time.Minute) {
			l.sweep()
		}
	}()
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if !l.Allow(keyFn(r)) {
				Err(w, NewError(http.StatusTooManyRequests, "RATE_LIMITED", "请求过于频繁，请稍后再试"))
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

// ClientIP 取真实客户端 IP（nginx 透传 X-Real-IP 优先）。
func ClientIP(r *http.Request) string {
	if x := r.Header.Get("X-Real-IP"); x != "" {
		return x
	}
	if x := r.Header.Get("X-Forwarded-For"); x != "" {
		return x
	}
	return r.RemoteAddr
}
