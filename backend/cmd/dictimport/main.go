// 导入 JMdict-simplified JSON（jmdict-eng-*.json）到 dict_entry / dict_form。
// 用法：在 backend/ 目录下 go run ./cmd/dictimport [-reset] <jmdict.json>
// -reset 会清空词典词条（dict_zh 中文缓存随外键级联删除），重新全量导入。
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"strconv"
	"time"

	"github.com/greatgeek/TingShuo/backend/pkg/envcfg"
	"github.com/jackc/pgx/v5"
)

type jform struct {
	Common bool     `json:"common"`
	Text   string   `json:"text"`
	Tags   []string `json:"tags"`
}

type jentry struct {
	ID    string          `json:"id"`
	Kanji []jform         `json:"kanji"`
	Kana  []jform         `json:"kana"`
	Sense json.RawMessage `json:"sense"`
}

type entryRow struct {
	id     int64
	senses []byte
	common bool
}

type formRow struct {
	entryID int64
	form    string
	kind    string
	common  bool
	tags    []string
}

const batchSize = 5000

func main() {
	reset := flag.Bool("reset", false, "清空现有词典数据后重新导入")
	flag.Parse()
	if flag.NArg() < 1 {
		fmt.Fprintln(os.Stderr, "用法: go run ./cmd/dictimport [-reset] <jmdict.json>")
		os.Exit(1)
	}
	path := flag.Arg(0)

	_ = envcfg.Load(".env.dev", ".env")
	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		fmt.Fprintln(os.Stderr, "DATABASE_URL 未设置")
		os.Exit(1)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()

	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		fmt.Fprintln(os.Stderr, "connect:", err)
		os.Exit(1)
	}
	defer conn.Close(context.Background())

	if *reset {
		if _, err := conn.Exec(ctx, `TRUNCATE dict_entry CASCADE`); err != nil {
			fmt.Fprintln(os.Stderr, "truncate:", err)
			os.Exit(1)
		}
		fmt.Println("已清空 dict_entry（含 dict_form / dict_zh）")
	}

	f, err := os.Open(path)
	if err != nil {
		fmt.Fprintln(os.Stderr, "open:", err)
		os.Exit(1)
	}
	defer f.Close()

	dec := json.NewDecoder(f)
	tok, err := dec.Token()
	if err != nil || tok != json.Delim('{') {
		fmt.Fprintln(os.Stderr, "不是有效的 JMdict JSON 对象")
		os.Exit(1)
	}

	found := false
	total, forms, commonN, skipped := 0, 0, 0, 0
	for dec.More() {
		keyTok, err := dec.Token()
		if err != nil {
			fmt.Fprintln(os.Stderr, "读取键:", err)
			os.Exit(1)
		}
		key, _ := keyTok.(string)
		if key != "words" {
			var skip json.RawMessage
			if err := dec.Decode(&skip); err != nil {
				fmt.Fprintln(os.Stderr, "跳过字段 "+key+":", err)
				os.Exit(1)
			}
			continue
		}
		found = true
		if _, err := dec.Token(); err != nil { // '['
			fmt.Fprintln(os.Stderr, "读取 words:", err)
			os.Exit(1)
		}
		entries := make([]entryRow, 0, batchSize)
		formRows := make([]formRow, 0, batchSize*2)
		flush := func() error {
			if len(entries) == 0 {
				return nil
			}
			tx, err := conn.Begin(ctx)
			if err != nil {
				return err
			}
			defer tx.Rollback(ctx)
			if _, err := tx.CopyFrom(ctx, pgx.Identifier{"dict_entry"},
				[]string{"id", "senses", "common"},
				pgx.CopyFromSlice(len(entries), func(i int) ([]any, error) {
					e := entries[i]
					return []any{e.id, e.senses, e.common}, nil
				})); err != nil {
				return fmt.Errorf("copy dict_entry: %w", err)
			}
			if _, err := tx.CopyFrom(ctx, pgx.Identifier{"dict_form"},
				[]string{"entry_id", "form", "kind", "common", "tags"},
				pgx.CopyFromSlice(len(formRows), func(i int) ([]any, error) {
					r := formRows[i]
					return []any{r.entryID, r.form, r.kind, r.common, r.tags}, nil
				})); err != nil {
				return fmt.Errorf("copy dict_form: %w", err)
			}
			return tx.Commit(ctx)
		}

		for dec.More() {
			var je jentry
			if err := dec.Decode(&je); err != nil {
				fmt.Fprintln(os.Stderr, "解析词条:", err)
				os.Exit(1)
			}
			id, err := strconv.ParseInt(je.ID, 10, 64)
			if err != nil || id <= 0 || len(je.Sense) == 0 {
				skipped++
				continue
			}
			isCommon := false
			for _, k := range je.Kanji {
				if k.Common {
					isCommon = true
				}
			}
			for _, k := range je.Kana {
				if k.Common {
					isCommon = true
				}
			}
			entries = append(entries, entryRow{id: id, senses: je.Sense, common: isCommon})
			if isCommon {
				commonN++
			}
			seen := map[string]bool{}
			addForm := func(fs []jform, kind string) {
				for _, fm := range fs {
					if fm.Text == "" {
						continue
					}
					key := kind + "\x00" + fm.Text
					if seen[key] {
						continue
					}
					seen[key] = true
					tags := fm.Tags
					if tags == nil {
						tags = []string{}
					}
					formRows = append(formRows, formRow{
						entryID: id, form: fm.Text, kind: kind, common: fm.Common, tags: tags,
					})
					forms++
				}
			}
			addForm(je.Kanji, "k")
			addForm(je.Kana, "r")
			total++
			if len(entries) >= batchSize {
				if err := flush(); err != nil {
					fmt.Fprintln(os.Stderr, "导入:", err)
					os.Exit(1)
				}
				entries = entries[:0]
				formRows = formRows[:0]
			}
		}
		if _, err := dec.Token(); err != nil { // ']'
			fmt.Fprintln(os.Stderr, "读取 words 结束:", err)
			os.Exit(1)
		}
		if err := flush(); err != nil {
			fmt.Fprintln(os.Stderr, "导入:", err)
			os.Exit(1)
		}
	}
	if !found {
		fmt.Fprintln(os.Stderr, "JSON 中没有 words 数组")
		os.Exit(1)
	}

	var dbEntries, dbForms, dbCommon int
	if err := conn.QueryRow(ctx, `SELECT count(*), count(*) FILTER (WHERE common) FROM dict_entry`).
		Scan(&dbEntries, &dbCommon); err != nil {
		fmt.Fprintln(os.Stderr, "count dict_entry:", err)
		os.Exit(1)
	}
	if err := conn.QueryRow(ctx, `SELECT count(*) FROM dict_form`).Scan(&dbForms); err != nil {
		fmt.Fprintln(os.Stderr, "count dict_form:", err)
		os.Exit(1)
	}
	fmt.Printf("解析 %d 条，跳过 %d 条；本次词形 %d；库内词条=%d（常用=%d）词形=%d\n",
		total, skipped, forms, dbEntries, dbCommon, dbForms)
}
