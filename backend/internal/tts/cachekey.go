// Package tts 实现句级 TTS 的缓存键（FR-3.5：同句同参数只生成一次，
// 缓存键全局唯一——不同用户、不同课程的相同句子命中同一条资产，§5.2）。
package tts

import (
	"crypto/sha256"
	"encoding/hex"
	"strconv"
	"strings"
)

// Params 是参与 cache_key 的 TTS 生成参数。
type Params struct {
	Voice     string  // 音色：Nanami / Keita / ...
	Rate      float64 // 语速档位：0.85 / 1.0 / 1.15（FR-3.4）
	Style     string  // SSML express-as 风格（P1，空表示默认）
	EngineVer string  // 引擎版本（换版本后强制重新生成）
}

// CacheKey 返回 hash(sentence_norm + voice + rate + style + engine_ver) 的十六进制串。
func CacheKey(sentence string, p Params) string {
	raw := strings.Join([]string{
		NormalizeSentence(sentence),
		p.Voice,
		formatRate(p.Rate),
		p.Style,
		p.EngineVer,
	}, "\x1f")
	sum := sha256.Sum256([]byte(raw))
	return hex.EncodeToString(sum[:])
}

// NormalizeSentence 规范化句子文本：合并连续空白、去首尾空白。
// 保证 LLM 不同次输出仅空白差异的句子不重复计费。
func NormalizeSentence(s string) string {
	return strings.Join(strings.Fields(s), " ")
}

// formatRate 语速统一格式化为两位小数（0.85 / 1.00 / 1.15），
// 使 1.0 与 1.00 等数值写法命中同一缓存键。
func formatRate(r float64) string {
	return strconv.FormatFloat(r, 'f', 2, 64)
}
