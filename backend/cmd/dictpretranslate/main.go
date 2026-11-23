// 常用词预翻：为 JMdict 常用词批量生成中文义项（不生成例句）。
// 用法：go run ./cmd/dictpretranslate -limit 100 -batch 20
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"syscall"

	"github.com/greatgeek/TingShuo/backend/internal/dict"
	"github.com/greatgeek/TingShuo/backend/pkg/db"
	"github.com/greatgeek/TingShuo/backend/pkg/envcfg"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "dictpretranslate:", err)
		os.Exit(1)
	}
}

func run() error {
	_ = envcfg.Load(".env.dev", ".env")
	dsn, err := envcfg.MustGet("DATABASE_URL")
	if err != nil {
		return err
	}
	limit := flag.Int("limit", 15000, "最多预翻多少个常用词")
	batch := flag.Int("batch", 20, "每批提交多少个词")
	flag.Parse()

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	pool, err := db.Connect(ctx, dsn)
	if err != nil {
		return err
	}
	defer pool.Close()

	h := dict.NewHandler(pool, nil, nil)
	stats, err := h.Pretranslate(ctx, dict.PretranslateOptions{
		Limit: *limit,
		Batch: *batch,
		Logf:  log.Printf,
	})
	fmt.Printf("成功 %d，失败 %d，LLM 调用 %d 次\n", stats.Done, stats.Failed, stats.Calls)
	return err
}
