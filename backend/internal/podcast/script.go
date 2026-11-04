package podcast

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// 脚本生成（v13 内容层）：LLM 出「行级脚本」，语言分配在本层决定（服务端持 LLM key，
// 与 internal/generate/job.go 同模式：env LLM_BASE_URL / LLM_API_KEY / LLM_MODEL）。

type WordBrief struct {
	Surface string `json:"surface"`
	Reading string `json:"reading"`
	Meaning string `json:"meaning"`
}

type SentenceBrief struct {
	Text        string `json:"text"`
	Translation string `json:"translation"`
}

// Input 为一集播客的内容原料（来自一课）。
type Input struct {
	Words     []WordBrief     `json:"words"`
	Sentences []SentenceBrief `json:"sentences"`
}

// LoadLessonInput 取一课的关键词与例句作为播客原料（归属校验：课必须属于该用户）。
func LoadLessonInput(ctx context.Context, pool *pgxpool.Pool, userID, lessonID string) (Input, error) {
	var n int
	if err := pool.QueryRow(ctx, `SELECT count(1) FROM daily_lesson WHERE id=$1 AND user_id=$2`, lessonID, userID).Scan(&n); err != nil || n == 0 {
		return Input{}, fmt.Errorf("课程不存在或不属于当前用户")
	}
	var in Input
	rows, err := pool.Query(ctx, `
		SELECT w.surface, coalesce(w.reading,''), coalesce(w.meaning,'')
		FROM lesson_word lw JOIN word w ON w.id = lw.word_id
		WHERE lw.lesson_id = $1
		ORDER BY (w.mastery = 'key') DESC, w.surface
		LIMIT 8`, lessonID)
	if err != nil {
		return Input{}, err
	}
	defer rows.Close()
	for rows.Next() {
		var w WordBrief
		if err := rows.Scan(&w.Surface, &w.Reading, &w.Meaning); err != nil {
			return Input{}, err
		}
		in.Words = append(in.Words, w)
	}
	if err := rows.Err(); err != nil {
		return Input{}, err
	}
	srows, err := pool.Query(ctx, `
		SELECT text, coalesce(translation,'') FROM sentence
		WHERE lesson_id = $1 ORDER BY sort_order LIMIT 6`, lessonID)
	if err != nil {
		return Input{}, err
	}
	defer srows.Close()
	for srows.Next() {
		var s SentenceBrief
		if err := srows.Scan(&s.Text, &s.Translation); err != nil {
			return Input{}, err
		}
		in.Sentences = append(in.Sentences, s)
	}
	return in, srows.Err()
}

// LoadDirInput 取生词本目录（单元）的词与例句作为播客原料（v0.3.1：生词本横条入口）。
func LoadDirInput(ctx context.Context, pool *pgxpool.Pool, userID, dirID string) (Input, error) {
	var n int
	if err := pool.QueryRow(ctx, `
		SELECT count(1) FROM notebook_dir d
		JOIN notebook nb ON nb.id = d.notebook_id
		WHERE d.id = $1 AND nb.user_id = $2`, dirID, userID).Scan(&n); err != nil || n == 0 {
		return Input{}, fmt.Errorf("目录不存在或不属于当前用户")
	}
	var in Input
	rows, err := pool.Query(ctx, `
		SELECT w.surface, coalesce(w.reading,''), coalesce(w.meaning,'')
		FROM notebook_word nw JOIN word w ON w.id = nw.word_id
		WHERE nw.dir_id = $1 ORDER BY nw.sort_order LIMIT 8`, dirID)
	if err != nil {
		return Input{}, err
	}
	defer rows.Close()
	for rows.Next() {
		var w WordBrief
		if err := rows.Scan(&w.Surface, &w.Reading, &w.Meaning); err != nil {
			return Input{}, err
		}
		in.Words = append(in.Words, w)
	}
	if err := rows.Err(); err != nil {
		return Input{}, err
	}
	srows, err := pool.Query(ctx, `
		SELECT ns.text, coalesce(ns.translation,'')
		FROM notebook_sentence ns JOIN notebook_word nw ON nw.id = ns.notebook_word_id
		WHERE nw.dir_id = $1 ORDER BY nw.sort_order, ns.sort_order LIMIT 6`, dirID)
	if err != nil {
		return Input{}, err
	}
	defer srows.Close()
	for srows.Next() {
		var s SentenceBrief
		if err := srows.Scan(&s.Text, &s.Translation); err != nil {
			return Input{}, err
		}
		in.Sentences = append(in.Sentences, s)
	}
	return in, srows.Err()
}

const scriptSystem = `你是日语教学播客《通勤日语电台》的脚本作者。两位主持：思思（女，role=a）、xudong（男，role=b），两人均能自然地说中文与日语。
硬约束：
1. 只输出 JSON：{"lines":[{"role":"a","text":"..."}]}，行数 8—24。
2. 中文行面向中国学习者讲解；日语行是示范/跟读/场景原声，必须短（不超过 24 个字符，超长就拆成两行）。
3. 每个日语行前后必须有中文行包裹（cue 句，如"听——""跟我念一遍——""意思是……"）。
4. 捧哏节奏以 a/b 交替为主；同一 role 连续不超过 2 行。
5. 覆盖输入关键词：每个词至少一次日语示范 + 一次中文释义；例句可拆成短引用，不整句照读。
6. 日语用です・ます体或自然口语短句；text 内不写假名注音、不写角色名。
7. 首行 role=a 中文开场；末行中文收尾（如"下期见！"）。`

// BuildScript 调 LLM 生成脚本；schema 不通过自动重试 1 次（仓库既有模式）。
// onProg 非 nil 时流式回调（思考字数, 正文字数）（进度流用）。
func BuildScript(ctx context.Context, in Input, onProg func(think, content int)) ([]Line, error) {
	var lastErr error
	for attempt := 0; attempt < 2; attempt++ {
		lines, err := buildOnce(ctx, in, lastErr, onProg)
		if err == nil {
			return lines, nil
		}
		lastErr = err
	}
	return nil, lastErr
}

func buildOnce(ctx context.Context, in Input, prev error, onProg func(think, content int)) ([]Line, error) {
	var sb strings.Builder
	sb.WriteString("关键词：\n")
	for _, w := range in.Words {
		sb.WriteString(fmt.Sprintf("- %s（%s）%s\n", w.Surface, w.Reading, w.Meaning))
	}
	sb.WriteString("例句：\n")
	for _, s := range in.Sentences {
		sb.WriteString(fmt.Sprintf("- %s / %s\n", s.Text, s.Translation))
	}
	if prev != nil {
		sb.WriteString("\n上一次输出未通过校验：" + prev.Error() + "。请修正后重新输出。")
	}

	payload := map[string]any{
		"model": os.Getenv("LLM_MODEL"),
		"messages": []map[string]any{
			{"role": "system", "content": scriptSystem},
			{"role": "user", "content": sb.String()},
		},
		"temperature":     0.7,
		"reasoning_split": true, // M3 思考内容独立字段，避免混入 content 污染 JSON
		"stream":          true, // 流式收稿：写稿阶段也有真实进度
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	target := strings.TrimSuffix(os.Getenv("LLM_BASE_URL"), "/") + "/chat/completions"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, target, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+os.Getenv("LLM_API_KEY"))
	cli := &http.Client{Timeout: 180 * time.Second}
	resp, err := cli.Do(req)
	if err != nil {
		return nil, fmt.Errorf("LLM 请求失败: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("LLM 返回 %d", resp.StatusCode)
	}
	var cb strings.Builder
	var think int
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 256*1024), 2*1024*1024)
	lastEmit := time.Now()
	emitProg := func() {
		if onProg != nil && time.Since(lastEmit) > 500*time.Millisecond {
			lastEmit = time.Now()
			onProg(think, len([]rune(cb.String())))
		}
	}
	for sc.Scan() {
		line := sc.Text()
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if data == "[DONE]" {
			break
		}
		var chunk struct {
			Choices []struct {
				Delta struct {
					Content   string `json:"content"`
					Reasoning string `json:"reasoning_content"`
				} `json:"delta"`
			} `json:"choices"`
		}
		if err := json.Unmarshal([]byte(data), &chunk); err != nil {
			continue
		}
		if len(chunk.Choices) == 0 {
			continue
		}
		d := chunk.Choices[0].Delta
		if d.Reasoning != "" {
			think += len([]rune(d.Reasoning))
			emitProg()
		}
		if d.Content != "" {
			cb.WriteString(d.Content)
			emitProg()
		}
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	if onProg != nil {
		onProg(think, len([]rune(cb.String())))
	}
	content := strings.TrimSpace(cb.String())
	if content == "" {
		return nil, fmt.Errorf("LLM 无输出")
	}
	// 兜底：剥掉代码围栏与可能残留的思考标签，只取最外层 JSON 对象
	if i := strings.Index(content, "{"); i >= 0 {
		if j := strings.LastIndex(content, "}"); j > i {
			content = content[i : j+1]
		}
	}
	var parsed struct {
		Lines []Line `json:"lines"`
	}
	if err := json.Unmarshal([]byte(content), &parsed); err != nil {
		return nil, fmt.Errorf("脚本 JSON 解析失败: %w", err)
	}
	if err := ValidateScript(parsed.Lines); err != nil {
		return nil, err
	}
	return parsed.Lines, nil
}

// kanaCount 统计假名字符数。
// 日语行判定用「假名数 ≥ 4」而非「含假名」：中文讲解行常内嵌短引用（如"多い"2 假名），
// 若按含假名判定会把 28 字中文行误判为日语行并触发长度上限（2026-11-04 实测踩坑）。
func kanaCount(s string) int {
	n := 0
	for _, r := range s {
		if (r >= 0x3040 && r <= 0x30FF) || (r >= 0xFF66 && r <= 0xFF9F) {
			n++
		}
	}
	return n
}

// ValidateScript 内容层校验（v13）：行数/角色/日语行长度与占比/首末行语言。
func ValidateScript(lines []Line) error {
	if len(lines) < 8 || len(lines) > 24 {
		return fmt.Errorf("行数 %d 不在 8—24 范围", len(lines))
	}
	ja := 0
	for i, l := range lines {
		if l.Role != RoleA && l.Role != RoleB {
			return fmt.Errorf("第 %d 行 role 非法", i)
		}
		if strings.TrimSpace(l.Text) == "" {
			return fmt.Errorf("第 %d 行文本为空", i)
		}
		if kanaCount(l.Text) >= 4 {
			ja++
			if n := len([]rune(l.Text)); n > 24 {
				return fmt.Errorf("第 %d 行日语过长（%d 字 > 24），请拆成两个短句：%s", i, n, l.Text)
			}
		}
	}
	if ja < 2 {
		return fmt.Errorf("日语行 %d 少于 2", ja)
	}
	if ja*2 > len(lines) {
		return fmt.Errorf("日语行占比过高（%d/%d）", ja, len(lines))
	}
	if kanaCount(lines[0].Text) >= 4 || kanaCount(lines[len(lines)-1].Text) >= 4 {
		return fmt.Errorf("首末行必须为中文")
	}
	return nil
}
