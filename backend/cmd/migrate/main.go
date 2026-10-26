// 数据库迁移工具（部署文档 §4：go run ./cmd/migrate up）。
// 在 backend/ 目录下运行；迁移文件位于 migrations/*.up.sql，按文件名版本号顺序应用。
// 注意：破坏性变更一律走「加新列→双写→切读→删旧列」，不依赖 down 回滚。
package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/greatgeek/TingShuo/backend/pkg/envcfg"
	"github.com/jackc/pgx/v5"
)

const migrationsDir = "migrations"

type migration struct {
	version int64
	name    string
	path    string
}

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "migrate:", err)
		os.Exit(1)
	}
}

func run() error {
	_ = envcfg.Load(".env.dev", ".env")

	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		return fmt.Errorf("DATABASE_URL 未设置（检查环境变量或 .env.dev）")
	}

	cmd := "up"
	if len(os.Args) > 1 {
		cmd = os.Args[1]
	}
	switch cmd {
	case "up":
		return migrateUp(dsn)
	case "status":
		return status(dsn)
	default:
		return fmt.Errorf("未知子命令 %q（可用：up, status）", cmd)
	}
}

func connect(dsn string) (*pgx.Conn, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	return pgx.Connect(ctx, dsn)
}

func migrateUp(dsn string) error {
	files, err := migrationFiles()
	if err != nil {
		return err
	}
	if len(files) == 0 {
		return fmt.Errorf("%s 目录中没有迁移文件", migrationsDir)
	}

	conn, err := connect(dsn)
	if err != nil {
		return fmt.Errorf("连接数据库: %w", err)
	}
	defer conn.Close(context.Background())
	ctx := context.Background()

	if _, err := conn.Exec(ctx, `CREATE TABLE IF NOT EXISTS schema_migrations (
		version    BIGINT PRIMARY KEY,
		applied_at TIMESTAMPTZ NOT NULL DEFAULT now())`); err != nil {
		return fmt.Errorf("建 schema_migrations 表: %w", err)
	}

	applied, err := appliedVersions(ctx, conn)
	if err != nil {
		return err
	}

	n := 0
	for _, m := range files {
		if applied[m.version] {
			continue
		}
		sqlBytes, err := os.ReadFile(m.path)
		if err != nil {
			return err
		}
		fmt.Printf("→ 应用 %s ...\n", filepath.Base(m.path))
		// pgx 无参数 Exec 走 simple protocol，支持文件内多语句（迁移文件自带 BEGIN/COMMIT）
		if _, err := conn.Exec(ctx, string(sqlBytes)); err != nil {
			return fmt.Errorf("迁移 %s 失败: %w", m.name, err)
		}
		if _, err := conn.Exec(ctx,
			`INSERT INTO schema_migrations(version) VALUES ($1)`, m.version); err != nil {
			return fmt.Errorf("记录版本 %d: %w", m.version, err)
		}
		n++
	}
	if n == 0 {
		fmt.Println("数据库已是最新，无待应用迁移")
	} else {
		fmt.Printf("完成：应用了 %d 个迁移\n", n)
	}
	return nil
}

func status(dsn string) error {
	files, err := migrationFiles()
	if err != nil {
		return err
	}
	conn, err := connect(dsn)
	if err != nil {
		return fmt.Errorf("连接数据库: %w", err)
	}
	defer conn.Close(context.Background())

	applied, err := appliedVersions(context.Background(), conn)
	if err != nil {
		return err
	}
	for _, m := range files {
		mark := "  [ ] 待应用"
		if applied[m.version] {
			mark = "  [x] 已应用"
		}
		fmt.Printf("%s %s%s\n", mark, m.name, filepath.Ext(""))
	}
	return nil
}

// migrationFiles 扫描 migrations 目录中全部 *.up.sql，解析文件名前缀版本号并升序排列。
func migrationFiles() ([]migration, error) {
	entries, err := os.ReadDir(migrationsDir)
	if err != nil {
		return nil, err
	}
	var files []migration
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".up.sql") {
			continue
		}
		base := strings.TrimSuffix(e.Name(), ".up.sql")
		verStr, _, ok := strings.Cut(base, "_")
		if !ok {
			return nil, fmt.Errorf("迁移文件名缺少版本前缀: %s", e.Name())
		}
		v, err := strconv.ParseInt(verStr, 10, 64)
		if err != nil {
			return nil, fmt.Errorf("迁移文件名版本号非法: %s", e.Name())
		}
		files = append(files, migration{version: v, name: base, path: filepath.Join(migrationsDir, e.Name())})
	}
	sort.Slice(files, func(i, j int) bool { return files[i].version < files[j].version })
	return files, nil
}

func appliedVersions(ctx context.Context, conn *pgx.Conn) (map[int64]bool, error) {
	rows, err := conn.Query(ctx, `SELECT version FROM schema_migrations`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	applied := make(map[int64]bool)
	for rows.Next() {
		var v int64
		if err := rows.Scan(&v); err != nil {
			return nil, err
		}
		applied[v] = true
	}
	return applied, rows.Err()
}
