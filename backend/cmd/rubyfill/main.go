// 回填生词本句子的振り仮名：notebook_sentence.ruby_tokens 为空的行，
// 用与课程句子同一套 kagome 分词（generate.BuildRubyTokens）生成后写回。幂等，可重复跑。
// 用法：在 backend/ 目录下 go run ./cmd/rubyfill（读 .env.dev 的 DATABASE_URL）。
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"time"

	"github.com/greatgeek/TingShuo/backend/internal/generate"
	"github.com/greatgeek/TingShuo/backend/pkg/envcfg"
	"github.com/jackc/pgx/v5/pgxpool"
)

func main() {
	_ = envcfg.Load(".env.dev", ".env")
	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		fmt.Fprintln(os.Stderr, "DATABASE_URL 未设置")
		os.Exit(1)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		fmt.Fprintln(os.Stderr, "connect:", err)
		os.Exit(1)
	}
	defer pool.Close()

	rows, err := pool.Query(ctx,
		`SELECT id, text FROM notebook_sentence WHERE ruby_tokens IS NULL`)
	if err != nil {
		fmt.Fprintln(os.Stderr, "query:", err)
		os.Exit(1)
	}
	type item struct {
		id   string
		text string
	}
	var items []item
	for rows.Next() {
		var it item
		if err := rows.Scan(&it.id, &it.text); err != nil {
			fmt.Fprintln(os.Stderr, "scan:", err)
			os.Exit(1)
		}
		items = append(items, it)
	}
	rows.Close()

	n := 0
	for _, it := range items {
		b, _ := json.Marshal(generate.BuildRubyTokens(it.text))
		if _, err := pool.Exec(ctx,
			`UPDATE notebook_sentence SET ruby_tokens = $1 WHERE id = $2`, b, it.id); err != nil {
			fmt.Fprintln(os.Stderr, "update:", err)
			os.Exit(1)
		}
		n++
	}
	fmt.Printf("回填完成：%d 句\n", n)
}
