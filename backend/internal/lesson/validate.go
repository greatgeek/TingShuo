// 词单校验与规范化（FR-1.5 预检：去重；Q-002：reading 空串方案）。
//
// 2026-10-26 需求变更（用户决策）：词数不再设学习性下限——1 个词也可以提交生成；
// 仅保留防滥用上限 MaxWords。原「不足 5 词报错」改为软性建议（前端文案）。
package lesson

import (
	"fmt"
	"strings"

	"github.com/greatgeek/TingShuo/backend/internal/httpx"
)

// MinWords / MaxWords：至少 1 词；MaxWords 仅为防滥用上限（超大粘贴）。
const (
	MinWords = 1
	MaxWords = 200
)

// WordInput 是 POST /lessons 的单词输入。
type WordInput struct {
	Surface string `json:"surface"`
	Reading string `json:"reading"`
	Pos     string `json:"pos"`
	Meaning string `json:"meaning"`
	IsKey   bool   `json:"is_key"`
}

// ValidateWords 校验并规范化词单，返回唯一化后的列表：
// - surface 必填、去首尾空白；空行跳过（FR-1.2 批量粘贴的空行容忍）
// - reading/pos/meaning 去空白；reading 为空 → 空串入库（空串参与 UNIQUE，
//   解决 PG 中 NULL 不参与唯一约束导致同词重复入库的问题，见开发问题记录 Q-002）
// - 按 (surface, reading) 去重，后出现的覆盖先出现的（保留最新标记）
func ValidateWords(raw []WordInput) ([]WordInput, *httpx.APIError) {
	if len(raw) > MaxWords {
		return nil, httpx.BadRequest("WORD_COUNT_INVALID",
			fmt.Sprintf("词数超限：至多 %d 个（FR-1.5 预检），当前 %d 个", MaxWords, len(raw)))
	}

	seen := make(map[string]int)
	out := make([]WordInput, 0, len(raw))
	for _, w := range raw {
		w.Surface = strings.TrimSpace(w.Surface)
		if w.Surface == "" {
			continue
		}
		w.Reading = strings.TrimSpace(w.Reading)
		w.Pos = strings.TrimSpace(w.Pos)
		w.Meaning = strings.TrimSpace(w.Meaning)

		key := w.Surface + "\x1f" + w.Reading
		if i, ok := seen[key]; ok {
			out[i] = w // 重复词：后到的覆盖（视为用户修正）
		} else {
			seen[key] = len(out)
			out = append(out, w)
		}
	}

	if len(out) < MinWords {
		return nil, httpx.BadRequest("WORD_COUNT_INVALID",
			fmt.Sprintf("有效词数不足：至少 %d 个（FR-1.5 预检），当前 %d 个", MinWords, len(out)))
	}
	return out, nil
}
