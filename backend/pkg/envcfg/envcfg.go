// Package envcfg 提供极简 .env 配置加载（部署文档附录 A 格式）。
// 已存在的环境变量不会被覆盖：真实环境变量优先于 .env 文件。
package envcfg

import (
	"bufio"
	"fmt"
	"os"
	"strings"
)

// Load 按顺序尝试加载 .env 文件，文件不存在则跳过。
func Load(paths ...string) error {
	for _, p := range paths {
		if err := loadFile(p); err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return err
		}
	}
	return nil
}

func loadFile(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()

	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			continue // 格式异常的行宽容跳过，不阻塞启动
		}
		k = strings.TrimSpace(k)
		v = strings.Trim(strings.TrimSpace(v), `"'`) // 支持值带引号
		if k != "" && os.Getenv(k) == "" {
			if err := os.Setenv(k, v); err != nil {
				return err
			}
		}
	}
	return sc.Err()
}

// MustGet 返回必须存在的环境变量，缺失时报错（Key/连接串缺失启动即报错，部署文档 §5）。
func MustGet(key string) (string, error) {
	v := os.Getenv(key)
	if v == "" {
		return "", fmt.Errorf("环境变量 %s 未设置（检查 .env.dev 或进程环境）", key)
	}
	return v, nil
}
