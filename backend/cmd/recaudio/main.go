// 上传课文本整段音频到对象存储（音频桶）。
// 用法: 在 backend/ 目录下 go run ./cmd/recaudio <mp3Dir>
// 文件名 lesson-05.mp3 → 对象 key recitation-lesson-05.mp3
package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/greatgeek/TingShuo/backend/pkg/envcfg"
	"github.com/greatgeek/TingShuo/backend/pkg/storage"
)

func main() {
	_ = envcfg.Load(".env.dev", ".env")
	dir := ""
	if len(os.Args) > 1 {
		dir = os.Args[1]
	}
	if dir == "" {
		fmt.Fprintln(os.Stderr, "用法: go run ./cmd/recaudio <mp3Dir>")
		os.Exit(1)
	}
	store, err := storage.New(
		os.Getenv("S3_ENDPOINT"), os.Getenv("S3_ACCESS_KEY"), os.Getenv("S3_SECRET_KEY"),
		os.Getenv("S3_BUCKET_AUDIO"), os.Getenv("S3_BUCKET_RECORDING"))
	if err != nil {
		fmt.Fprintln(os.Stderr, "storage:", err)
		os.Exit(1)
	}
	files, err := filepath.Glob(filepath.Join(dir, "lesson-*.mp3"))
	if err != nil || len(files) == 0 {
		fmt.Fprintln(os.Stderr, "未找到 lesson-*.mp3:", err)
		os.Exit(1)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	for _, f := range files {
		data, err := os.ReadFile(f)
		if err != nil {
			fmt.Fprintln(os.Stderr, "读取:", err)
			os.Exit(1)
		}
		key := "recitation-" + strings.TrimSuffix(filepath.Base(f), ".mp3")
		if err := store.PutAudio(ctx, key, data); err != nil {
			fmt.Fprintln(os.Stderr, "上传:", key, err)
			os.Exit(1)
		}
		fmt.Printf("已上传 %s (%d bytes)\n", storage.AudioKey(key), len(data))
	}
}
