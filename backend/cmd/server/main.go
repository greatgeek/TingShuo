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
	"github.com/greatgeek/TingShuo/backend/internal/generate"
	"github.com/greatgeek/TingShuo/backend/internal/httpx"
	"github.com/greatgeek/TingShuo/backend/internal/lesson"
	"github.com/greatgeek/TingShuo/backend/internal/review"
	"github.com/greatgeek/TingShuo/backend/internal/user"
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

	signer := jwtw.New(jwtSecret)
	authH := auth.NewHandler(pool, signer)
	userH := user.NewHandler(pool)
	lessonH := lesson.NewHandler(pool)
	reviewH := review.NewHandler(pool)
	genH := generate.NewHandler(pool)

	r := chi.NewRouter()
	r.Use(middleware.Recoverer)
	r.Use(middleware.RequestID)

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

			sec.Post("/lessons", lessonH.Create)
			sec.Get("/lessons", lessonH.List)
			sec.Get("/lessons/{id}", lessonH.Get)
			sec.Post("/lessons/{id}/materials", genH.Upload)

			sec.Post("/reviews", reviewH.Add)
			sec.Get("/reviews/due", reviewH.Due)
			sec.Post("/reviews/{id}/result", reviewH.SubmitResult)
			sec.Post("/reviews/{id}/master", reviewH.Master)
		})
	})

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
