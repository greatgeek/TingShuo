package wordstudy

// 句式条目补句：〜さんのおかげです 这类带占位符的"词"，直接念出来不明所以
// （2026-11-06 用户报障）。打开目录时后台触发一次：LLM 把 〜 换成具体人名/事物
// 写一条完整日常的例句+中文翻译，落 notebook_sentence（src='llm-pattern'，界面标「补」），
// 之后听力题挖空句式部分就有意义可听。失败/校验不过静默跳过，维持"退选义"兜底。

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strings"
	"sync"

	"github.com/go-chi/chi/v5"
	"github.com/greatgeek/TingShuo/backend/internal/auth"
	"github.com/greatgeek/TingShuo/backend/internal/generate"
	"github.com/greatgeek/TingShuo/backend/internal/httpx"
	"github.com/greatgeek/TingShuo/backend/pkg/llmclient"
	"github.com/greatgeek/TingShuo/backend/pkg/ulid"
	"github.com/jackc/pgx/v5/pgxpool"
)

const patternSrc = "llm-pattern"

func containsKana(s string) bool {
	for _, r := range s {
		if (r >= 0x3041 && r <= 0x3096) || (r >= 0x30A1 && r <= 0x30FA) {
			return true
		}
	}
	return false
}

// patternTail 句式真正要考的部分：〜さんのおかげです → さんのおかげです
func patternTail(surface string) string { return strings.TrimLeft(surface, "〜~…○□") }

type patternSent struct {
	Text        string `json:"text"`
	Translation string `json:"translation"`
}

// validPatternSent 补句门槛：句式部分在句里、长度合适、无占位符残留、翻译是纯中文
func validPatternSent(s patternSent, tail string) error {
	t := strings.TrimSpace(s.Text)
	if !strings.Contains(t, tail) {
		return fmt.Errorf("句子未包含句式部分 %q", tail)
	}
	n := len([]rune(t))
	if n < 6 || n > 40 {
		return fmt.Errorf("句子长度 %d 超出 6-40", n)
	}
	if patternMarkRe.MatchString(t) {
		return fmt.Errorf("句子仍含占位符")
	}
	if !strings.HasSuffix(t, "。") && !strings.HasSuffix(t, "？") && !strings.HasSuffix(t, "!") {
		return fmt.Errorf("句子未以句末标点收尾")
	}
	tr := strings.TrimSpace(s.Translation)
	if tr == "" || len([]rune(tr)) > 60 {
		return fmt.Errorf("翻译缺失或过长")
	}
	if containsKana(tr) {
		return fmt.Errorf("翻译混入假名")
	}
	if patternMarkRe.MatchString(tr) {
		return fmt.Errorf("翻译含占位符")
	}
	return nil
}

func patternPrompt(surface, meaning string) string {
	return fmt.Sprintf(`你是日语教材编辑。给句式「%s」（中文义：%s）写一条自然、完整、日常的日语例句。
要求：把开头的 〜 替换成具体的人名或事物（如 田中／先生／このアプリ），句式部分原样保留；
句子 6-30 字、以。结尾；不要出现 〜 … ○ 等占位符；不要解释、不要多余文字。
再给一句中文翻译（翻译里不要出现日文）。
只输出 JSON：{"text":"...","translation":"..."}`, surface, meaning)
}

// genPatternSentence 调 LLM 出补句；解析失败返回零值由调用方跳过
func genPatternSentence(ctx context.Context, surface, meaning string) (patternSent, error) {
	cli := llmclient.New(
		strings.TrimSuffix(os.Getenv("LLM_BASE_URL"), "/"),
		os.Getenv("LLM_API_KEY"),
		os.Getenv("LLM_MODEL"),
	)
	if cli == nil {
		return patternSent{}, fmt.Errorf("LLM 未配置")
	}
	content, _, _, err := cli.ChatJSON(ctx, patternPrompt(surface, meaning))
	if err != nil {
		return patternSent{}, err
	}
	var s patternSent
	if err := json.Unmarshal([]byte(llmclient.ExtractJSON(content)), &s); err != nil {
		return patternSent{}, err
	}
	return s, nil
}

// EnrichDir POST /word-study/dir/{dir_id}/enrich —— 目录内句式条目补具体例句。
// 前端打开目录时 fire-and-forget 调一次；已有补句的跳过，单次最多补 8 条。
func (h *Handler) EnrichDir(w http.ResponseWriter, r *http.Request) {
	userID := auth.FromContext(r.Context())
	dirID := chi.URLParam(r, "dir_id")
	rows, err := h.pool.Query(r.Context(), `
		SELECT nw.id, w.surface, COALESCE(w.meaning, '')
		FROM notebook_word nw
		JOIN notebook nb ON nb.id = nw.notebook_id
		JOIN word w      ON w.id = nw.word_id
		WHERE nw.dir_id = $1 AND nb.user_id = $2
		  AND w.surface ~ '^[〜~…○□]'
		  AND NOT EXISTS (
		    SELECT 1 FROM notebook_sentence s
		    WHERE s.notebook_word_id = nw.id AND s.src = $3)
		ORDER BY nw.sort_order LIMIT 8`, dirID, userID, patternSrc)
	if err != nil {
		httpx.Err(w, httpx.Internal(err.Error()))
		return
	}
	type target struct {
		linkID, surface, meaning string
	}
	var targets []target
	for rows.Next() {
		var t target
		if err := rows.Scan(&t.linkID, &t.surface, &t.meaning); err != nil {
			continue
		}
		targets = append(targets, t)
	}
	rows.Close()

	ctx := r.Context()
	var mu sync.Mutex
	done := 0
	sem := make(chan struct{}, 2)
	var wg sync.WaitGroup
	for _, t := range targets {
		wg.Add(1)
		go func(t target) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			if h.enrichOne(ctx, h.pool, t.linkID, t.surface, t.meaning) {
				mu.Lock()
				done++
				mu.Unlock()
			}
		}(t)
	}
	wg.Wait()
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"enriched": done, "targets": len(targets)})
}

func (h *Handler) enrichOne(ctx context.Context, pool *pgxpool.Pool, linkID, surface, meaning string) bool {
	tail := patternTail(surface)
	if tail == "" {
		return false
	}
	s, err := genPatternSentence(ctx, surface, meaning)
	if err != nil || validPatternSent(s, tail) != nil {
		return false
	}
	ruby, _ := json.Marshal(generate.BuildRubyTokens(s.Text))
	var sort int
	_ = pool.QueryRow(ctx,
		`SELECT COALESCE(MAX(sort_order), -1) + 1 FROM notebook_sentence WHERE notebook_word_id = $1`,
		linkID).Scan(&sort)
	_, err = pool.Exec(ctx, `
		INSERT INTO notebook_sentence (id, notebook_word_id, text, ruby_tokens, translation, src, sort_order)
		VALUES ($1, $2, $3, $4, $5, $6, $7)
		ON CONFLICT (notebook_word_id, text) DO NOTHING`,
		ulid.MustNew(), linkID, strings.TrimSpace(s.Text), ruby, strings.TrimSpace(s.Translation), patternSrc, sort)
	return err == nil
}
