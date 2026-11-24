// 批量生成生词常用词组：为 word 表中还没有 word_phrase 的词生成 5 条短搭配
// （日文+假名+中文，至少 2 条动宾），按 surface/reading/pos/meaning 去重后批量请求大模型并入库。
// -supplement 则给已有词组的词补足到 5 条，已有条目保持不变。
// 用法（backend/ 目录）：go run ./cmd/phrasefill -workers=6 -batch=8 [-limit=0] [-redo] [-supplement] [-failout=file.json]
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/greatgeek/TingShuo/backend/internal/wordphrase"
	"github.com/greatgeek/TingShuo/backend/pkg/envcfg"
	"github.com/greatgeek/TingShuo/backend/pkg/llmclient"
	"github.com/greatgeek/TingShuo/backend/pkg/ulid"
)

type group struct {
	info wordphrase.WordInfo
	ids  []string
}

type failure struct {
	Surface  string   `json:"surface"`
	Reading  string   `json:"reading"`
	Problems []string `json:"problems"`
}

func main() {
	workers := flag.Int("workers", 6, "并发请求数")
	batch := flag.Int("batch", 8, "每次请求的词数")
	limit := flag.Int("limit", 0, "只处理前 N 个去重词组（0=全部）")
	redo := flag.Bool("redo", false, "重新生成已有缓存的词")
	supplement := flag.Bool("supplement", false, "为已有词组的词补足到 5 条（优先动宾），保留已有条目")
	failout := flag.String("failout", "", "把生成不合格的词写入该 JSON 文件")
	effort := flag.String("effort", "", "思考强度：low/medium（空=模型默认，仅离线批量用）")
	flag.Parse()
	if *effort != "" {
		os.Setenv("LLM_REASONING_EFFORT", *effort)
	}

	_ = envcfg.Load(".env.dev", ".env")
	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		fmt.Fprintln(os.Stderr, "DATABASE_URL 未设置")
		os.Exit(1)
	}
	if os.Getenv("LLM_API_KEY") == "" || os.Getenv("LLM_BASE_URL") == "" {
		fmt.Fprintln(os.Stderr, "LLM_API_KEY / LLM_BASE_URL 未设置")
		os.Exit(1)
	}
	model := os.Getenv("LLM_MODEL")

	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Hour)
	defer cancel()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		fmt.Fprintln(os.Stderr, "connect:", err)
		os.Exit(1)
	}
	defer pool.Close()

	if *supplement {
		client := llmclient.New(os.Getenv("LLM_BASE_URL"), os.Getenv("LLM_API_KEY"), model)
		if err := runSupplement(ctx, pool, client, model, *workers, *batch, *limit, *failout); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		return
	}

	groups, err := loadGroups(ctx, pool, *redo)
	if err != nil {
		fmt.Fprintln(os.Stderr, "load words:", err)
		os.Exit(1)
	}
	if *limit > 0 && len(groups) > *limit {
		groups = groups[:*limit]
	}
	if len(groups) == 0 {
		fmt.Println("没有需要生成词组的词")
		return
	}
	fmt.Printf("待生成：%d 个去重词组（%d 个词条），每批 %d 词，并发 %d\n",
		len(groups), countIDs(groups), *batch, *workers)

	ex, err := loadExamples(ctx, pool)
	if err != nil {
		fmt.Fprintln(os.Stderr, "load examples:", err)
		os.Exit(1)
	}
	for i := range groups {
		if len(groups[i].ids) > 0 {
			groups[i].info.ExampleText = ex[groups[i].ids[0]]
		}
	}

	client := llmclient.New(os.Getenv("LLM_BASE_URL"), os.Getenv("LLM_API_KEY"), model)
	jobs := make(chan []group)
	var (
		done, saved, empty, partial int64
		promptTok, compTok         int64
		mu                         sync.Mutex
		fails                      []failure
		wg                         sync.WaitGroup
	)
	start := time.Now()

	for i := 0; i < *workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for gs := range jobs {
				itemsByIDX, stats, err := wordphrase.Generate(ctx, client, infos(gs), false)
				atomic.AddInt64(&promptTok, int64(stats.PromptTokens))
				atomic.AddInt64(&compTok, int64(stats.CompletionTokens))
				if err != nil {
					mu.Lock()
					for _, g := range gs {
						fails = append(fails, failure{g.info.Surface, g.info.Reading, []string{err.Error()}})
					}
					mu.Unlock()
					atomic.AddInt64(&done, int64(len(gs)))
					continue
				}
				// 不足 5 条的词单独重试一次
				var retryIdx []int
				var retryWords []wordphrase.WordInfo
				for i, g := range gs {
					if len(itemsByIDX[i].Items) < wordphrase.WantCount {
						retryIdx = append(retryIdx, i)
						retryWords = append(retryWords, g.info)
					}
				}
				if len(retryWords) > 0 {
					res2, s2, err2 := wordphrase.Generate(ctx, client, retryWords, true)
					atomic.AddInt64(&promptTok, int64(s2.PromptTokens))
					atomic.AddInt64(&compTok, int64(s2.CompletionTokens))
					if err2 == nil {
						for j, i := range retryIdx {
							if r2, ok := res2[j]; ok && len(r2.Items) > len(itemsByIDX[i].Items) {
								itemsByIDX[i] = r2
							}
						}
					}
				}
				logCall(ctx, pool, model, stats, 0)
				for i, g := range gs {
					res := itemsByIDX[i]
					atomic.AddInt64(&done, 1)
					if len(res.Items) == 0 {
						atomic.AddInt64(&empty, 1)
						mu.Lock()
						fails = append(fails, failure{g.info.Surface, g.info.Reading, res.Problems})
						mu.Unlock()
						continue
					}
					if len(res.Items) < wordphrase.WantCount {
						atomic.AddInt64(&partial, 1)
					}
					for _, id := range g.ids {
						if err := wordphrase.Save(ctx, pool, id, res.Items, model); err != nil {
							fmt.Fprintf(os.Stderr, "save %s(%s): %v\n", g.info.Surface, id, err)
						} else {
							atomic.AddInt64(&saved, 1)
						}
					}
				}
				if n := atomic.LoadInt64(&done); n%160 == 0 {
					fmt.Printf("进度 %d/%d，已写 %d 词，空结果 %d，不足 5 条 %d，用时 %s\n",
						n, len(groups), atomic.LoadInt64(&saved), atomic.LoadInt64(&empty),
						atomic.LoadInt64(&partial), time.Since(start).Round(time.Second))
				}
			}
		}()
	}
	for i := 0; i < len(groups); i += *batch {
		j := i + *batch
		if j > len(groups) {
			j = len(groups)
		}
		jobs <- groups[i:j]
	}
	close(jobs)
	wg.Wait()

	fmt.Printf("完成：去重词组 %d，写词条 %d，空结果 %d，不足 5 条 %d；token 输入 %d / 输出 %d；用时 %s\n",
		len(groups), atomic.LoadInt64(&saved), atomic.LoadInt64(&empty), atomic.LoadInt64(&partial),
		atomic.LoadInt64(&promptTok), atomic.LoadInt64(&compTok), time.Since(start).Round(time.Second))
	if *failout != "" && len(fails) > 0 {
		b, _ := json.MarshalIndent(fails, "", "  ")
		if err := os.WriteFile(*failout, b, 0o644); err != nil {
			fmt.Fprintln(os.Stderr, "write failout:", err)
		} else {
			fmt.Printf("不合格词已写入 %s（%d 个）\n", *failout, len(fails))
		}
	}
}

type supGroup struct {
	info     wordphrase.WordInfo
	existing []wordphrase.Item
	need     int
	ids      []string
}

// runSupplement 给已有词组的词补足到 WantCount 条：已有条目原样保留，
// 只补写缺口（优先动宾），合并后写回。
func runSupplement(ctx context.Context, pool *pgxpool.Pool, client *llmclient.Client, model string, workers, batch, limit int, failout string) error {
	groups, err := loadSupplementGroups(ctx, pool)
	if err != nil {
		return fmt.Errorf("load words: %w", err)
	}
	if limit > 0 && len(groups) > limit {
		groups = groups[:limit]
	}
	if len(groups) == 0 {
		fmt.Println("没有需要补写词组的词")
		return nil
	}
	total := 0
	for _, g := range groups {
		total += len(g.ids)
	}
	fmt.Printf("待补写：%d 个去重词组（%d 个词条），每批 %d 词，并发 %d\n", len(groups), total, batch, workers)

	jobs := make(chan []supGroup)
	var (
		done, saved, empty, partial int64
		promptTok, compTok         int64
		mu                         sync.Mutex
		fails                      []failure
		wg                         sync.WaitGroup
	)
	start := time.Now()
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for gs := range jobs {
				inputs := make([]wordphrase.SupplementInput, len(gs))
				for i, g := range gs {
					inputs[i] = wordphrase.SupplementInput{Info: g.info, Existing: g.existing, Need: g.need}
				}
				res, stats, err := callSupplement(ctx, client, inputs, false)
				atomic.AddInt64(&promptTok, int64(stats.PromptTokens))
				atomic.AddInt64(&compTok, int64(stats.CompletionTokens))
				if err != nil {
					mu.Lock()
					for _, g := range gs {
						fails = append(fails, failure{g.info.Surface, g.info.Reading, []string{err.Error()}})
					}
					mu.Unlock()
					atomic.AddInt64(&done, int64(len(gs)))
					continue
				}
				var retryIdx []int
				var retryInputs []wordphrase.SupplementInput
				for i, g := range gs {
					if len(res[i].Items) < g.need {
						retryIdx = append(retryIdx, i)
						retryInputs = append(retryInputs, inputs[i])
					}
				}
				if len(retryInputs) > 0 {
					res2, s2, err2 := callSupplement(ctx, client, retryInputs, true)
					atomic.AddInt64(&promptTok, int64(s2.PromptTokens))
					atomic.AddInt64(&compTok, int64(s2.CompletionTokens))
					if err2 == nil {
						for j, i := range retryIdx {
							if r2, ok := res2[j]; ok && len(r2.Items) > len(res[i].Items) {
								res[i] = r2
							}
						}
					}
				}
				logCall(ctx, pool, model, stats, 0)
				for i, g := range gs {
					r := res[i]
					atomic.AddInt64(&done, 1)
					if len(r.Items) == 0 {
						atomic.AddInt64(&empty, 1)
						mu.Lock()
						fails = append(fails, failure{g.info.Surface, g.info.Reading, r.Problems})
						mu.Unlock()
						continue
					}
					if len(r.Items) < g.need {
						atomic.AddInt64(&partial, 1)
					}
					merged := mergeItems(g.existing, r.Items)
					for _, id := range g.ids {
						if err := wordphrase.Save(ctx, pool, id, merged, model); err != nil {
							fmt.Fprintf(os.Stderr, "save %s(%s): %v\n", g.info.Surface, id, err)
						} else {
							atomic.AddInt64(&saved, 1)
						}
					}
				}
				if n := atomic.LoadInt64(&done); n%160 == 0 {
					fmt.Printf("进度 %d/%d，已写 %d 词，空结果 %d，补写不足 %d，用时 %s\n",
						n, len(groups), atomic.LoadInt64(&saved), atomic.LoadInt64(&empty),
						atomic.LoadInt64(&partial), time.Since(start).Round(time.Second))
				}
			}
		}()
	}
	for i := 0; i < len(groups); i += batch {
		j := i + batch
		if j > len(groups) {
			j = len(groups)
		}
		jobs <- groups[i:j]
	}
	close(jobs)
	wg.Wait()

	fmt.Printf("完成：去重词组 %d，写词条 %d，空结果 %d，补写不足 %d；token 输入 %d / 输出 %d；用时 %s\n",
		len(groups), atomic.LoadInt64(&saved), atomic.LoadInt64(&empty), atomic.LoadInt64(&partial),
		atomic.LoadInt64(&promptTok), atomic.LoadInt64(&compTok), time.Since(start).Round(time.Second))
	if failout != "" && len(fails) > 0 {
		b, _ := json.MarshalIndent(fails, "", "  ")
		if err := os.WriteFile(failout, b, 0o644); err != nil {
			fmt.Fprintln(os.Stderr, "write failout:", err)
		} else {
			fmt.Printf("不合格词已写入 %s（%d 个）\n", failout, len(fails))
		}
	}
	return nil
}

// callSupplement 调用 Supplement，遇到 429 限速时退避重试。
func callSupplement(ctx context.Context, client *llmclient.Client, inputs []wordphrase.SupplementInput, retry bool) (map[int]wordphrase.Result, wordphrase.GenStats, error) {
	var (
		res   map[int]wordphrase.Result
		stats wordphrase.GenStats
		err   error
	)
	for attempt := 0; ; attempt++ {
		res, stats, err = wordphrase.Supplement(ctx, client, inputs, retry)
		if err == nil || !strings.Contains(err.Error(), "429") || attempt >= 4 {
			return res, stats, err
		}
		d := time.Duration(15<<attempt) * time.Second
		fmt.Printf("触发限速，%s 后重试（第 %d 次）\n", d, attempt+1)
		select {
		case <-ctx.Done():
			return res, stats, ctx.Err()
		case <-time.After(d):
		}
	}
}

func mergeItems(existing, add []wordphrase.Item) []wordphrase.Item {
	return wordphrase.MergeItems(existing, add)
}

func loadSupplementGroups(ctx context.Context, pool *pgxpool.Pool) ([]supGroup, error) {
	rows, err := pool.Query(ctx, `
		SELECT w.id, coalesce(w.surface,''), coalesce(w.reading,''), coalesce(w.pos,''), coalesce(w.meaning,''), p.payload
		FROM word w JOIN word_phrase p ON p.word_id = w.id
		ORDER BY w.created_at, w.id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	type rawWord struct {
		id, surface, reading, pos, meaning string
		payload                            []byte
	}
	var all []rawWord
	surfaceReadings := map[string]map[string]bool{}
	for rows.Next() {
		var r rawWord
		if err := rows.Scan(&r.id, &r.surface, &r.reading, &r.pos, &r.meaning, &r.payload); err != nil {
			return nil, err
		}
		all = append(all, r)
		if r.reading != "" {
			if surfaceReadings[r.surface] == nil {
				surfaceReadings[r.surface] = map[string]bool{}
			}
			surfaceReadings[r.surface][r.reading] = true
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	var groups []supGroup
	index := map[string]int{}
	for _, r := range all {
		if r.reading == "" {
			if set := surfaceReadings[r.surface]; len(set) == 1 {
				for k := range set {
					r.reading = k
				}
			}
		}
		var items []wordphrase.Item
		if json.Unmarshal(r.payload, &items) != nil || len(items) == 0 {
			continue
		}
		need := wordphrase.WantCount - len(items)
		if need <= 0 {
			continue
		}
		texts := make([]string, len(items))
		for i, it := range items {
			texts[i] = it.Text
		}
		key := r.surface + "\x00" + r.reading + "\x00" + r.pos + "\x00" + r.meaning + "\x00" + strings.Join(texts, "\x01")
		if i, ok := index[key]; ok {
			groups[i].ids = append(groups[i].ids, r.id)
			continue
		}
		index[key] = len(groups)
		groups = append(groups, supGroup{
			info:     wordphrase.WordInfo{Surface: r.surface, Reading: r.reading, Pos: r.pos, Meaning: r.meaning},
			existing: items,
			need:     need,
			ids:      []string{r.id},
		})
	}
	return groups, nil
}

func infos(gs []group) []wordphrase.WordInfo {
	out := make([]wordphrase.WordInfo, len(gs))
	for i, g := range gs {
		out[i] = g.info
	}
	return out
}

func countIDs(gs []group) int {
	n := 0
	for _, g := range gs {
		n += len(g.ids)
	}
	return n
}

func loadGroups(ctx context.Context, pool *pgxpool.Pool, redo bool) ([]group, error) {
	where := ""
	if !redo {
		where = "LEFT JOIN word_phrase p ON p.word_id = w.id WHERE p.word_id IS NULL"
	}
	rows, err := pool.Query(ctx, `
		SELECT w.id, w.surface, coalesce(w.reading,''), coalesce(w.pos,''), coalesce(w.meaning,'')
		FROM word w `+where+`
		ORDER BY w.created_at, w.id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	type rawWord struct{ id, surface, reading, pos, meaning string }
	var all []rawWord
	surfaceReadings := map[string]map[string]bool{}
	for rows.Next() {
		var r rawWord
		if err := rows.Scan(&r.id, &r.surface, &r.reading, &r.pos, &r.meaning); err != nil {
			return nil, err
		}
		all = append(all, r)
		if r.reading != "" {
			if surfaceReadings[r.surface] == nil {
				surfaceReadings[r.surface] = map[string]bool{}
			}
			surfaceReadings[r.surface][r.reading] = true
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	var groups []group
	index := map[string]int{}
	for _, r := range all {
		// 老词条可能没有读音：同词面在库内只有唯一读音时继承之，避免「後→ご」这类
		// 词典误读被当作词条读音强制到所有词组上。
		if r.reading == "" {
			if set := surfaceReadings[r.surface]; len(set) == 1 {
				for k := range set {
					r.reading = k
				}
			}
		}
		key := r.surface + "\x00" + r.reading + "\x00" + r.pos + "\x00" + r.meaning
		if i, ok := index[key]; ok {
			groups[i].ids = append(groups[i].ids, r.id)
			continue
		}
		index[key] = len(groups)
		groups = append(groups, group{
			info: wordphrase.WordInfo{Surface: r.surface, Reading: r.reading, Pos: r.pos, Meaning: r.meaning},
			ids:  []string{r.id},
		})
	}
	return groups, nil
}

// loadExamples 每个词取最短的一条生词本例句作参考，帮助模型贴近本课语境。
func loadExamples(ctx context.Context, pool *pgxpool.Pool) (map[string]string, error) {
	rows, err := pool.Query(ctx, `
		SELECT DISTINCT ON (nw.word_id) nw.word_id, s.text
		FROM notebook_word nw
		JOIN notebook_sentence s ON s.notebook_word_id = nw.id
		ORDER BY nw.word_id, char_length(s.text)`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var id, text string
		if err := rows.Scan(&id, &text); err != nil {
			return nil, err
		}
		out[id] = text
	}
	return out, rows.Err()
}

func logCall(ctx context.Context, pool *pgxpool.Pool, model string, stats wordphrase.GenStats, ms int64) {
	_, _ = pool.Exec(ctx, `
		INSERT INTO llm_call_log (id, user_id, lesson_id, model, prompt_tokens, output_tokens, duration_ms)
		VALUES ($1, NULL, NULL, $2, $3, $4, $5)`,
		ulid.MustNew(), model, stats.PromptTokens, stats.CompletionTokens, ms)
}