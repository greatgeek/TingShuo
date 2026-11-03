package podcast

import (
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

const scriptSystem = `你是日语教学播客《通勤日语电台》的脚本作者。两位主持：思思（女，role=a）、xudong（男，role=b），两人均能自然地说中文与日语。
硬约束：
1. 只输出 JSON：{"lines":[{"role":"a","text":"..."}]}，行数 8—24。
2. 中文行面向中国学习者讲解；日语行是示范/跟读/场景原声，必须短（不超过 20 个字符）。
3. 每个日语行前后必须有中文行包裹（cue 句，如"听——""跟我念一遍——""意思是……"）。
4. 捧哏节奏以 a/b 交替为主；同一 role 连续不超过 2 行。
5. 覆盖输入关键词：每个词至少一次日语示范 + 一次中文释义；例句可拆成短引用，不整句照读。
6. 日语用です・ます体或自然口语短句；text 内不写假名注音、不写角色名。
7. 首行 role=a 中文开场；末行中文收尾（如"下期见！"）。`

// BuildScript 调 LLM 生成脚本；schema 不通过自动重试 1 次（仓库既有模式）。
func BuildScript(ctx context.Context, in Input) ([]Line, error) {
	var lastErr error
	for attempt := 0; attempt < 2; attempt++ {
		lines, err := buildOnce(ctx, in, lastErr)
		if err == nil {
			return lines, nil
		}
		lastErr = err
	}
	return nil, lastErr
}

func buildOnce(ctx context.Context, in Input, prev error) ([]Line, error) {
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
	var out struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, err
	}
	if len(out.Choices) == 0 {
		return nil, fmt.Errorf("LLM 无输出")
	}
	content := strings.TrimSpace(out.Choices[0].Message.Content)
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
		if HasJapanese(l.Text) {
			ja++
			if n := len([]rune(l.Text)); n > 20 {
				return fmt.Errorf("第 %d 行日语过长（%d 字 > 20）", i, n)
			}
		}
	}
	if ja < 2 {
		return fmt.Errorf("日语行 %d 少于 2", ja)
	}
	if ja*2 > len(lines) {
		return fmt.Errorf("日语行占比过高（%d/%d）", ja, len(lines))
	}
	if HasJapanese(lines[0].Text) || HasJapanese(lines[len(lines)-1].Text) {
		return fmt.Errorf("首末行必须为中文")
	}
	return nil
}
