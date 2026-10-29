// HTTP 服务入口（§6 API：REST + JSON，前缀 /api/v1，JWT Bearer 认证）。
// 启动：go run ./cmd/server（backend/ 目录下，读 .env.dev / .env）
package main

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"

	"github.com/greatgeek/TingShuo/backend/internal/auth"
	"github.com/greatgeek/TingShuo/backend/internal/dev"
	"github.com/greatgeek/TingShuo/backend/internal/favorite"
	"github.com/greatgeek/TingShuo/backend/internal/generate"
	"github.com/greatgeek/TingShuo/backend/internal/httpx"
	"github.com/greatgeek/TingShuo/backend/internal/lesson"
	"github.com/greatgeek/TingShuo/backend/internal/notebook"
	"github.com/greatgeek/TingShuo/backend/internal/review"
	"github.com/greatgeek/TingShuo/backend/internal/tts"
	"github.com/greatgeek/TingShuo/backend/internal/user"
	"github.com/greatgeek/TingShuo/backend/internal/word"
	"github.com/greatgeek/TingShuo/backend/internal/wordstudy"
	"github.com/greatgeek/TingShuo/backend/pkg/storage"
	"github.com/greatgeek/TingShuo/backend/pkg/ttsclient"
	"github.com/greatgeek/TingShuo/backend/pkg/db"
	"github.com/greatgeek/TingShuo/backend/pkg/envcfg"
	"github.com/greatgeek/TingShuo/backend/pkg/jwtw"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "server:", err)
		os.Exit(1)
	}
}

func run() error {
	_ = envcfg.Load(".env.dev", ".env")

	dsn, err := envcfg.MustGet("DATABASE_URL")
	if err != nil {
		return err
	}
	jwtSecret, err := envcfg.MustGet("JWT_SECRET")
	if err != nil {
		return err
	}
	listen, err := envcfg.MustGet("LISTEN_ADDR")
	if err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	pool, err := db.Connect(ctx, dsn)
	if err != nil {
		return err
	}
	defer pool.Close()

	// 上次进程残留的 running/pending 生成任务置 failed（上游连接随进程死亡不可恢复）
	generate.CleanupStale(ctx, pool)

	store, err := storage.New(
		mustEnv("S3_ENDPOINT"), mustEnv("S3_ACCESS_KEY"), mustEnv("S3_SECRET_KEY"),
		mustEnv("S3_BUCKET_AUDIO"), mustEnv("S3_BUCKET_RECORDING"))
	if err != nil {
		return err
	}

	azureTTS := ttsclient.New(mustEnv("AZURE_SPEECH_KEY"), mustEnv("AZURE_SPEECH_REGION"))

	signer := jwtw.New(jwtSecret)
	authH := auth.NewHandler(pool, signer)
	userH := user.NewHandler(pool)
	lessonH := lesson.NewHandler(pool)
	reviewH := review.NewHandler(pool)
	genH := generate.NewHandler(pool)
	jobH := generate.NewJobHandler(pool)
	ttsH := tts.NewHandler(pool, store, azureTTS)
	favH := favorite.NewHandler(pool)
	wordH := word.NewHandler(pool)

	r := chi.NewRouter()
	r.Use(middleware.Recoverer)
	r.Use(middleware.RequestID)
	r.Use(dev.Cors) // 本地开发 CORS（试用页同源时无感，保留给分离开发场景）

	r.Get("/healthz", func(w http.ResponseWriter, req *http.Request) {
		if err := pool.Ping(req.Context()); err != nil {
			httpx.WriteJSON(w, http.StatusServiceUnavailable, map[string]string{"status": "db_down"})
			return
		}
		httpx.WriteJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})

	r.Route("/api/v1", func(api chi.Router) {
		// 公开端点
		api.Post("/auth/register", authH.Register)
		api.Post("/auth/login", authH.Login)
		api.Post("/auth/refresh", authH.Refresh)

		// 鉴权端点
		api.Group(func(sec chi.Router) {
			sec.Use(authH.Middleware)
			sec.Get("/me", userH.Me)
			sec.Put("/me", userH.Update)

			sec.Get("/words/library", wordH.Library)
			sec.Get("/words/{id}/usage", wordH.Usage)

			wsH := wordstudy.NewHandler(pool, azureTTS)
			sec.Get("/word-study/status", wsH.Status)
			sec.Get("/word-study/new", wsH.NewWords)
			sec.Get("/word-study/due", wsH.DueWords)
			sec.Post("/word-study/{word_id}/result", wsH.Result)
			sec.Get("/word-study/{word_id}/audio", wsH.Audio)
			sec.Get("/word-study/{word_id}/sentences", wsH.NbSentences)

			nbH := notebook.NewHandler(pool)
			sec.Get("/notebooks", nbH.List)
			sec.Get("/notebooks/{id}/tree", nbH.Tree)
			sec.Get("/notebooks/dir/{dir_id}/words", nbH.DirWords)

			sec.Post("/audio/say", ttsH.Say)

			sec.Post("/favorites", favH.AddSentence)
			sec.Delete("/favorites/sentences/{sentence_id}", favH.RemoveSentence)
			sec.Get("/favorites/sentences", favH.ListSentences)
			sec.Post("/favorites/materials", favH.ToggleMaterial)
			sec.Get("/favorites/materials", favH.ListMaterials)

			sec.Post("/audio/check", ttsH.Check)
			sec.Post("/audio/synthesize", ttsH.Synthesize)
			sec.Post("/audio/presign", ttsH.Presign)
			sec.Post("/audio/register", ttsH.Register)
			sec.Get("/audio/{id}/file", ttsH.File)

			// 本地开发试用代理（生产 DEV_PROXY=false 关闭，见 internal/dev 注释）
			if dev.Enabled() {
				if devH, derr := dev.NewHandler(); derr == nil {
					sec.Get("/dev/config", devH.Config)
					sec.Post("/dev/llm/start", devH.StartLLM)
					sec.Get("/dev/llm/poll", devH.PollLLM)
					sec.Post("/dev/tts", devH.TTSProxy)
					sec.Post("/dev/upload", devH.Upload)
				} else {
					fmt.Fprintln(os.Stderr, "dev 代理未启用:", derr)
				}
			}

			sec.Post("/lessons", lessonH.Create)
			sec.Get("/lessons", lessonH.List)
			sec.Get("/lessons/{id}", lessonH.Get)
			sec.Delete("/lessons/{id}", lessonH.Delete)
			sec.Post("/lessons/{id}/materials", genH.Upload)

			// 生成任务服务端化（部署文档 §4）：服务端持 Key 代生成，任务落 generation_job
			sec.Get("/config", jobH.Config)
			sec.Post("/generate/start", jobH.Start)
			sec.Get("/generate/poll", jobH.Poll)

			sec.Post("/reviews", reviewH.Add)
			sec.Get("/reviews/due", reviewH.Due)
			sec.Get("/reviews/pool", reviewH.Pool)
			sec.Post("/reviews/{id}/result", reviewH.SubmitResult)
			sec.Post("/reviews/{id}/master", reviewH.Master)
		})
	})

	// 桌面前端静态托管：优先仓库根的 web/（正式代码库），回退 backend/web/
	// no-cache：前端迭代期保证浏览器拿到最新 JS/CSS
	for _, dir := range []string{"../web", "web"} {
		if _, err := os.Stat(dir); err == nil {
			r.Handle("/*", noCache(http.FileServer(http.Dir(dir))))
			break
		}
	}

	srv := &http.Server{
		Addr:              listen,
		Handler:           r,
		ReadHeaderTimeout: 10 * time.Second,
	}
	fmt.Printf("TingShuo server 监听 %s\n", listen)
	go func() {
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdown)
	}()
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		return err
	}
	return nil
}

// mustEnv 取必填环境变量，缺失即退出（部署文档 §5：缺 Key 启动即报错）。
func mustEnv(key string) string {
	v, err := envcfg.MustGet(key)
	if err != nil {
		fmt.Fprintln(os.Stderr, "server:", err)
		os.Exit(1)
	}
	return v
}

// noCache 静态资源禁缓存中间件（前端迭代期）。
func noCache(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-cache")
		next.ServeHTTP(w, r)
	})
}
