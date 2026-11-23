// 常用词批量预翻：只生成中文义项并写入 dict_zh，例句仍由详情页首次打开时按需补。
package dict

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/greatgeek/TingShuo/backend/pkg/llmclient"
	"github.com/greatgeek/TingShuo/backend/pkg/ulid"
)

// flexInt 兼容模型把 id 输出成数字或字符串
type flexInt int

func (n *flexInt) UnmarshalJSON(b []byte) error {
	b = bytes.TrimSpace(b)
	if len(b) > 0 && b[0] == '"' {
		var s string
		if err := json.Unmarshal(b, &s); err != nil {
			return err
		}
		v, err := strconv.Atoi(strings.TrimSpace(s))
		if err != nil {
			return err
		}
		*n = flexInt(v)
		return nil
	}
	var v int
	if err := json.Unmarshal(b, &v); err != nil {
		return err
	}
	*n = flexInt(v)
	return nil
}

type PretranslateOptions struct {
	Limit int
	Batch int
	Logf  func(format string, args ...any)
}

type PretranslateStats struct {
	Done   int
	Failed int
	Calls  int
}

type ptRow struct {
	ID      int64
	Senses  []byte
	Surface string
	Reading string
	Rlen    int
}

// Pretranslate 按「读音长度、词条 id」排序，批量生成常用词中文义项。
// 进度即 dict_zh 行，命令中断后重跑会自动跳过已完成的词条。
func (h *Handler) Pretranslate(ctx context.Context, opts PretranslateOptions) (PretranslateStats, error) {
	var stats PretranslateStats
	if opts.Batch <= 0 {
		opts.Batch = 20
	}
	if opts.Logf == nil {
		opts.Logf = func(string, ...any) {}
	}
	if os.Getenv("LLM_API_KEY") == "" || os.Getenv("LLM_BASE_URL") == "" {
		return stats, fmt.Errorf("LLM 未配置")
	}

	var remaining int
	if err := h.pool.QueryRow(ctx, `
		SELECT count(*) FROM dict_entry e
		WHERE e.common AND NOT EXISTS (SELECT 1 FROM dict_zh z WHERE z.entry_id = e.id)`).
		Scan(&remaining); err != nil {
		return stats, err
	}
	if opts.Limit > 0 && remaining > opts.Limit {
		remaining = opts.Limit
	}
	opts.Logf("待预翻 %d 词（每批 %d）", remaining, opts.Batch)

	start := time.Now()
	var lastRlen int
	var lastID int64
	consecutiveFail := 0
	skippedBatches := 0

	// 同一批连续失败多半是内容审核拦截或坏词，重试无意义：跳过并继续，
	// 连续跳过过多（上游故障）才终止，避免静默烧完全部词表。
	skipBatch := func(rows []ptRow, reason string) error {
		stats.Failed += len(rows)
		lastRlen = rows[len(rows)-1].Rlen
		lastID = rows[len(rows)-1].ID
		consecutiveFail = 0
		skippedBatches++
		opts.Logf("跳过本批 %d 词（%s），累计跳过 %d 批", len(rows), reason, skippedBatches)
		if skippedBatches >= 10 {
			return fmt.Errorf("连续跳过 %d 批，停止", skippedBatches)
		}
		return nil
	}

	for stats.Done+stats.Failed < remaining {
		want := opts.Batch
		if left := remaining - stats.Done - stats.Failed; left < want {
			want = left
		}
		rows, err := h.ptRows(ctx, lastRlen, lastID, want)
		if err != nil {
			return stats, err
		}
		if len(rows) == 0 {
			break
		}

		prompt := ptPrompt(rows)
		callStart := time.Now()
		content, pt, ct, err := h.client.ChatJSONNoThinking(ctx, prompt)
		stats.Calls++
		h.logSystemLLM(ctx, pt, ct, time.Since(callStart))
		if err != nil {
			consecutiveFail++
			opts.Logf("批次失败（连续 %d）：%v", consecutiveFail, err)
			if consecutiveFail >= 3 {
				if e := skipBatch(rows, "连续 3 次调用失败"); e != nil {
					return stats, e
				}
			}
			continue
		}

		blobs := ptItemBlobs(content)
		if len(blobs) == 0 {
			blobs = ptItemBlobs(llmclient.ExtractJSON(content))
		}
		got := map[int][]zhSense{}
		for _, blob := range blobs {
			var it struct {
				ID     flexInt   `json:"id"`
				Senses []zhSense `json:"senses"`
			}
			if err := json.Unmarshal([]byte(blob), &it); err != nil {
				continue // 单条坏格式只丢这一词，不连累整批
			}
			got[int(it.ID)] = it.Senses
		}
		if os.Getenv("DICT_DEBUG") != "" && len(got) == 0 {
			dbg := content
			if r := []rune(dbg); len(r) > 1500 {
				dbg = string(r[:1500]) + "…（截断）"
			}
			opts.Logf("调试：本批无有效条目，原始返回：%s", dbg)
		}
		batchDone, batchFail := 0, 0
		for i, row := range rows {
			senses := sanitizeZhSenses(got[i+1])
			if len(senses) == 0 {
				batchFail++
				continue
			}
			h.saveZh(ctx, row.ID, sensesHash(row.Senses), &zhContent{Senses: senses, Examples: []zhExample{}})
			batchDone++
		}
		stats.Done += batchDone
		if batchDone > 0 {
			// 只有出现有效结果才推进游标；整批失败原地重试，避免静默跳过 20 个词
			stats.Failed += batchFail
			lastRlen = rows[len(rows)-1].Rlen
			lastID = rows[len(rows)-1].ID
			consecutiveFail = 0
			skippedBatches = 0
		} else {
			consecutiveFail++ // 失败数在 skipBatch 时一次性计入，避免重试叠加
		}
		opts.Logf("进度 %d/%d（失败 %d），用时 %s", stats.Done, remaining, stats.Failed, time.Since(start).Round(time.Second))
		if consecutiveFail >= 3 {
			if e := skipBatch(rows, "连续 3 次无有效结果"); e != nil {
				return stats, e
			}
		}
	}
	opts.Logf("完成：成功 %d，失败 %d，调用 %d 次，用时 %s",
		stats.Done, stats.Failed, stats.Calls, time.Since(start).Round(time.Second))
	return stats, nil
}

func (h *Handler) ptRows(ctx context.Context, lastRlen int, lastID int64, limit int) ([]ptRow, error) {
	rows, err := h.pool.Query(ctx, `
		WITH f AS (
			SELECT entry_id,
			       COALESCE(min(length(form)) FILTER (WHERE kind = 'r'), 99) AS rlen
			FROM dict_form GROUP BY entry_id
		)
		SELECT e.id, e.senses, f.rlen,
		       COALESCE((SELECT form FROM dict_form WHERE entry_id = e.id AND kind = 'k'
		                 ORDER BY common DESC, length(form), form LIMIT 1), ''),
		       COALESCE((SELECT form FROM dict_form WHERE entry_id = e.id AND kind = 'r'
		                 ORDER BY common DESC, length(form), form LIMIT 1), '')
		FROM dict_entry e JOIN f ON f.entry_id = e.id
		WHERE e.common
		  AND NOT EXISTS (SELECT 1 FROM dict_zh z WHERE z.entry_id = e.id)
		  AND (f.rlen, e.id) > ($1, $2)
		ORDER BY f.rlen, e.id
		LIMIT $3`, lastRlen, lastID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []ptRow{}
	for rows.Next() {
		var r ptRow
		if err := rows.Scan(&r.ID, &r.Senses, &r.Rlen, &r.Surface, &r.Reading); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func ptPrompt(rows []ptRow) string {
	var b strings.Builder
	b.WriteString("你是日语词典编辑。请为下列日语词给出简明中文释义，供中文母语学习者使用。\n")
	for i, r := range rows {
		title := r.Surface
		if title == "" {
			title = r.Reading
		}
		if r.Reading != "" && r.Reading != title {
			title += "（" + r.Reading + "）"
		}
		b.WriteString(fmt.Sprintf("%d. %s\n", i+1, title))
		var jm []jmSense
		_ = json.Unmarshal(r.Senses, &jm)
		n := 0
		for _, s := range jm {
			glosses := []string{}
			for _, g := range s.Gloss {
				glosses = append(glosses, g.Text)
			}
			if len(glosses) == 0 {
				continue
			}
			label := posHint(s.PartOfSpeech)
			if len(s.Field) > 0 {
				label += "，领域：" + strings.Join(s.Field, "/")
			}
			if len(s.Misc) > 0 {
				label += "，标记：" + strings.Join(s.Misc, "/")
			}
			b.WriteString(fmt.Sprintf("   【%s】%s\n", label, strings.Join(glosses, "; ")))
			n++
			if n >= 4 {
				break
			}
		}
	}
	b.WriteString("要求：\n" +
		"1. 按给出的英文义项归纳，不要编造词条没有的义项；生僻、古语义项可省略；带领域标注的义项要保留领域（如【相扑】），不要按字面硬译。\n" +
		"2. 每个词给 1-3 个义项，每个义项 pos 用中文词性，zh 用 1-2 个简洁中文词/短语。\n" +
		"3. 只输出 JSON：{\"items\":[{\"id\":1,\"senses\":[{\"pos\":\"名词\",\"zh\":[\"房间\",\"屋子\"]}]}]}；id 必须与列表序号一致，不要解释。")
	return b.String()
}

// ptItemBlobs 从模型输出里逐个扫描 {...} 条目，绕开整体 JSON 被个别坏条目带崩、
// 或输出超长被截断的情况：坏一条只丢一条。
func ptItemBlobs(raw string) []string {
	b := []byte(raw)
	start := bytes.Index(b, []byte(`"items"`))
	if start < 0 {
		start = 0
	}
	var out []string
	depth := 0
	inStr, esc := false, false
	objStart := -1
	for i := start; i < len(b); i++ {
		c := b[i]
		if inStr {
			if esc {
				esc = false
			} else if c == '\\' {
				esc = true
			} else if c == '"' {
				inStr = false
			}
			continue
		}
		switch c {
		case '"':
			inStr = true
		case '{':
			if depth == 0 {
				objStart = i
			}
			depth++
		case '}':
			if depth > 0 {
				depth--
				if depth == 0 && objStart >= 0 {
					out = append(out, string(b[objStart:i+1]))
					objStart = -1
				}
			}
		}
	}
	return out
}

func (h *Handler) logSystemLLM(ctx context.Context, pt, ct int, dur time.Duration) {
	_, _ = h.pool.Exec(ctx, `
		INSERT INTO llm_call_log (id, user_id, lesson_id, model, prompt_tokens, output_tokens, duration_ms)
		VALUES ($1, NULL, NULL, $2, $3, $4, $5)`,
		ulid.MustNew(), os.Getenv("LLM_MODEL"), pt, ct, dur.Milliseconds())
}
