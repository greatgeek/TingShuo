// 回填振り仮名：sentence / notebook_sentence 两张表里 ruby_tokens 为空的行，
// 用与课程句子同一套 kagome 分词（generate.BuildRubyTokens）生成后写回。幂等，可重复跑。
// 全量重刷（读音纠正表更新后）：先 UPDATE 两表 SET ruby_tokens=NULL 再跑本工具。
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

	tables := []struct{ name, sel, upd string }{
		{"notebook_sentence",
			`SELECT id, text FROM notebook_sentence WHERE ruby_tokens IS NULL`,
			`UPDATE notebook_sentence SET ruby_tokens = $1 WHERE id = $2`},
		{"sentence",
			`SELECT id, text FROM sentence WHERE ruby_tokens IS NULL`,
			`UPDATE sentence SET ruby_tokens = $1 WHERE id = $2`},
	}
	type item struct {
		id   string
		text string
	}
	for _, tb := range tables {
		rows, err := pool.Query(ctx, tb.sel)
		if err != nil {
			fmt.Fprintln(os.Stderr, "query "+tb.name+":", err)
			os.Exit(1)
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
			if _, err := pool.Exec(ctx, tb.upd, b, it.id); err != nil {
				fmt.Fprintln(os.Stderr, "update:", err)
				os.Exit(1)
			}
			n++
		}
		fmt.Printf("回填完成：%s %d 句\n", tb.name, n)
	}
}