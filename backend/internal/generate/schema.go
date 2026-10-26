// Package generate 实现材料上传的 schema 校验与落库（FR-2 BYOK 形态：
// 客户端直连 LLM 生成 JSON → POST 到此 → 服务端校验 + kagome 振り仮名增强 + 落库）。
package generate

import (
	"encoding/json"
	"fmt"
	"strings"
)

// ---- §5.3 GenerationOutput 结构 ----

// GenerationOutput 是客户端 LLM 生成的完整材料包。
type GenerationOutput struct {
	Materials     []MaterialGen       `json:"materials"`
	QAItems        []QAItemGen         `json:"qa_items"`
	Substitutions  []SubstitutionGen   `json:"substitution_items"`
	WordOccurrence map[string]WordOcc `json:"word_occurrence"`
}

// MaterialGen 是一段材料（对话或短文）。ID 为客户端临时 ID（mid），落库时换成服务端 ULID。
type MaterialGen struct {
	ID        string        `json:"id"`
	Type      string        `json:"type"` // dialogue | passage
	Scene     string        `json:"scene"`
	Title     string        `json:"title"`
	RoleA     *string       `json:"role_a"`
	RoleB     *string       `json:"role_b"`
	SortOrder int           `json:"sort_order"`
	Sentences []SentenceGen `json:"sentences"`
}

// SentenceGen 是一句话。ID 为客户端临时 ID（sid）。
type SentenceGen struct {
	ID          string  `json:"id"`
	SortOrder   int     `json:"sort_order"`
	Speaker     *string `json:"speaker"`
	Text        string  `json:"text"`
	Kana        string  `json:"kana"` // LLM 整句假名：仅作交叉校验参考（FR-2.12）
	Translation string  `json:"translation"`
	ParaNo      int     `json:"para_no"`
}

type QAItemGen struct {
	Prompt    string `json:"prompt"`
	Answer    string `json:"answer"`
	PromptZh  string `json:"prompt_translation"`
}

type SubstitutionGen struct {
	WordID    string `json:"word_id"`
	PromptZh string `json:"prompt_zh"`
	Answer    string `json:"answer"`
}

// WordOccurrence 复现表条目：数组 = 出现的句 ID 列表；"skipped: 原因" = 无法自然使用（FR-2.8）。
type WordOcc struct {
	Sentences  []string
	Skipped    bool
	SkipReason string
}

// UnmarshalJSON 兼容两种形态：["s1","s2"] 或 "skipped: 专有名词不适合本场景"。
func (o *WordOcc) UnmarshalJSON(b []byte) error {
	s := strings.TrimSpace(string(b))
	if strings.HasPrefix(s, `"skipped:`) || strings.HasPrefix(s, `"skipped：`) {
		var raw string
		if err := json.Unmarshal(b, &raw); err != nil {
			return err
		}
		o.Skipped = true
		o.SkipReason = strings.TrimSpace(strings.TrimPrefix(raw, "skipped:"))
		return nil
	}
	return json.Unmarshal(b, &o.Sentences)
}

// ---- 校验（AC-2.1/2.2/2.3）----

const (
	MinSentencesPerMaterial = 8  // §5.3：对话每段 8—12 句、短文 8—12 句
	MaxSentencesPerMaterial = 12
	QAItemCount             = 5 // FR-2.4
	SubstitutionCount       = 5 // FR-2.5
)

// Validate 校验材料包：words 为本课词集合（wordID → isKey）。
// 返回问题列表（空 = 通过）。
func (out GenerationOutput) Validate(words map[string]bool) []string {
	var problems []string

	// 1. 材料与句子
	if len(out.Materials) == 0 {
		problems = append(problems, "materials 为空")
	}
	sidSet := map[string]bool{}
	for _, m := range out.Materials {
		switch m.Type {
		case "dialogue", "passage":
		default:
			problems = append(problems, fmt.Sprintf("材料 %q 的 type 非法：%q（须 dialogue/passage）", m.ID, m.Type))
		}
		if n := len(m.Sentences); n < MinSentencesPerMaterial || n > MaxSentencesPerMaterial {
			problems = append(problems, fmt.Sprintf("材料 %q 句数 %d 超出 %d—%d 范围（§5.3）", m.ID, n, MinSentencesPerMaterial, MaxSentencesPerMaterial))
		}
		for _, s := range m.Sentences {
			if strings.TrimSpace(s.Text) == "" {
				problems = append(problems, fmt.Sprintf("材料 %q 中存在空句子（id=%s）", m.ID, s.ID))
			}
			if strings.TrimSpace(s.ID) == "" {
				problems = append(problems, fmt.Sprintf("材料 %q 中句子缺少 id", m.ID))
				continue
			}
			if sidSet[s.ID] {
				problems = append(problems, fmt.Sprintf("句子临时 ID 重复：%s", s.ID))
			}
			sidSet[s.ID] = true
		}
	}

	// 2. 问答与造句数量（FR-2.4/2.5）
	if len(out.QAItems) != QAItemCount {
		problems = append(problems, fmt.Sprintf("qa_items 数量 %d ≠ %d", len(out.QAItems), QAItemCount))
	}
	for i, qa := range out.QAItems {
		if qa.Prompt == "" || qa.Answer == "" {
			problems = append(problems, fmt.Sprintf("qa_items[%d] 缺少 prompt 或 answer", i))
		}
	}
	if len(out.Substitutions) != SubstitutionCount {
		problems = append(problems, fmt.Sprintf("substitution_items 数量 %d ≠ %d", len(out.Substitutions), SubstitutionCount))
	}
	for i, sub := range out.Substitutions {
		if sub.WordID == "" || sub.PromptZh == "" || sub.Answer == "" {
			problems = append(problems, fmt.Sprintf("substitution_items[%d] 字段不完整", i))
		}
	}

	// 3. 词覆盖（AC-2.2）：全部词有交代；非 skipped 的引用句存在
	for wid, isKey := range words {
		occ, ok := out.WordOccurrence[wid]
		if !ok {
			problems = append(problems, fmt.Sprintf("word_occurrence 缺少词 %s（AC-2.2）", wid))
			continue
		}
		if occ.Skipped {
			if occ.SkipReason == "" {
				problems = append(problems, fmt.Sprintf("词 %s 标记 skipped 但未说明原因（FR-2.8）", wid))
			}
			continue
		}
		if len(occ.Sentences) == 0 {
			problems = append(problems, fmt.Sprintf("非 skipped 词 %s 的复现句列表为空（AC-2.2）", wid))
			continue
		}
		for _, sid := range occ.Sentences {
			if !sidSet[sid] {
				problems = append(problems, fmt.Sprintf("词 %s 引用了不存在的句子 %s", wid, sid))
			}
		}
		if isKey && len(occ.Sentences) < 2 {
			// 重点词出现 2 次是 FR-2.2 要求，偶发不达标记为提示性问题（客户端可重试）
			problems = append(problems, fmt.Sprintf("重点词 %s 仅出现 %d 次（FR-2.2 要求 2 次）", wid, len(occ.Sentences)))
		}
	}
	for wid := range out.WordOccurrence {
		if _, ok := words[wid]; !ok {
			problems = append(problems, fmt.Sprintf("word_occurrence 含未知词 ID：%s", wid))
		}
	}
	return problems
}
