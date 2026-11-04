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
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// 脚本生成（v0.4.0 长片）：分段写稿——开场导览 / 每 3 词一段深度讲解 / 收尾复习。
// 一次让 LLM 吐几百行 JSON 必被输出上限截断，分段还顺带给出「第 n/N 段」的真实进度，
// 以及段级重试（某段校验失败只重写那一段，不废掉整集）。
// 防重复做法：每段带「已讲词 + 已用例句」清单和上一段末尾两行，而不是把前文全文塞回去
// （podcastfy 用全文拼接，长集会把上下文撑爆）。
// 服务端持 LLM key（env LLM_BASE_URL / LLM_API_KEY / LLM_MODEL），与 internal/generate/job.go 同模式。

const (
	wordsPerSeg     = 3  // 每段讲几个词
	maxSentPerWord  = 2  // 每词最多带几句例句入料（多了会摊薄讲解）
	minLinesPerWord = 9  // 每词最少行数：少于这个数说明讲解被压缩成"一句带过"
	maxLinesPerWord = 28 // 每词最多行数：留出余量但挡住失控
	maxTotalLines   = 1500
	maxJaLineRunes  = 16 // 日语单行上限：耳朵一次只吃得下一小口，整句要拆片段
	maxZhLineRunes  = 26 // 中文单行上限：一次只说一个点，像说话不像念稿
	maxJaRepeat     = 2  // 同一句日语在一段里最多出现次数（示范+收尾各一遍，禁止三遍）
)

type WordBrief struct {
	Surface   string          `json:"surface"`
	Reading   string          `json:"reading"`
	Meaning   string          `json:"meaning"`
	Sentences []SentenceBrief `json:"sentences,omitempty"`
}

type SentenceBrief struct {
	Text        string `json:"text"`
	Translation string `json:"translation"`
}

// Input 为一集播客的内容原料：一个目录（或一课）的全部词，每词带自己的例句。
type Input struct {
	Title string      `json:"title"`
	Words []WordBrief `json:"words"`
}

// LoadLessonInput 取一课的全部词与每词例句（归属校验：课必须属于该用户）。
func LoadLessonInput(ctx context.Context, pool *pgxpool.Pool, userID, lessonID string) (Input, error) {
	var title string
	err := pool.QueryRow(ctx,
		`SELECT to_char(lesson_date,'YYYY年MM月DD日') FROM daily_lesson WHERE id=$1 AND user_id=$2`,
		lessonID, userID).Scan(&title)
	if err != nil {
		return Input{}, fmt.Errorf("课程不存在或不属于当前用户")
	}
	var (
		in  = Input{Title: title + " 这一课"}
		ids []string
	)
	rs, err := pool.Query(ctx, `
		SELECT w.id, w.surface, coalesce(w.reading,''), coalesce(w.meaning,'')
		FROM lesson_word lw JOIN word w ON w.id = lw.word_id
		WHERE lw.lesson_id = $1 AND NOT lw.skipped
		ORDER BY lw.is_key DESC, w.surface`, lessonID)
	if err != nil {
		return Input{}, err
	}
	for rs.Next() {
		var id string
		var w WordBrief
		if err := rs.Scan(&id, &w.Surface, &w.Reading, &w.Meaning); err != nil {
			rs.Close()
			return Input{}, err
		}
		ids = append(ids, id)
		in.Words = append(in.Words, w)
	}
	err = rs.Err()
	rs.Close()
	if err != nil {
		return Input{}, err
	}
	sents, err := sentencesByOwner(ctx, pool, `
		SELECT ws.word_id, s.text, coalesce(s.translation,'')
		FROM word_sentence ws JOIN sentence s ON s.id = ws.sentence_id
		WHERE ws.lesson_id = $1 ORDER BY s.sort_order`, lessonID)
	if err != nil {
		return Input{}, err
	}
	attach(&in, ids, sents)
	return in, nil
}

// LoadDirInput 取生词本目录（单元）的全部词与每词例句。
func LoadDirInput(ctx context.Context, pool *pgxpool.Pool, userID, dirID string) (Input, error) {
	var title string
	err := pool.QueryRow(ctx, `
		SELECT d.name FROM notebook_dir d
		JOIN notebook nb ON nb.id = d.notebook_id
		WHERE d.id = $1 AND nb.user_id = $2`, dirID, userID).Scan(&title)
	if err != nil {
		return Input{}, fmt.Errorf("目录不存在或不属于当前用户")
	}
	var (
		in  = Input{Title: title}
		ids []string
	)
	rs, err := pool.Query(ctx, `
		SELECT nw.id, w.surface, coalesce(w.reading,''), coalesce(w.meaning,'')
		FROM notebook_word nw JOIN word w ON w.id = nw.word_id
		WHERE nw.dir_id = $1 ORDER BY nw.sort_order`, dirID)
	if err != nil {
		return Input{}, err
	}
	for rs.Next() {
		var id string
		var w WordBrief
		if err := rs.Scan(&id, &w.Surface, &w.Reading, &w.Meaning); err != nil {
			rs.Close()
			return Input{}, err
		}
		ids = append(ids, id)
		in.Words = append(in.Words, w)
	}
	err = rs.Err()
	rs.Close()
	if err != nil {
		return Input{}, err
	}
	sents, err := sentencesByOwner(ctx, pool, `
		SELECT ns.notebook_word_id, ns.text, coalesce(ns.translation,'')
		FROM notebook_sentence ns JOIN notebook_word nw ON nw.id = ns.notebook_word_id
		WHERE nw.dir_id = $1 ORDER BY nw.sort_order, ns.sort_order`, dirID)
	if err != nil {
		return Input{}, err
	}
	attach(&in, ids, sents)
	return in, nil
}

// sentencesByOwner 按 owner id 收集例句，每个 owner 最多 maxSentPerWord 句。
// owner 在课程路径是 word_id，在目录路径是 notebook_word_id。
func sentencesByOwner(ctx context.Context, pool *pgxpool.Pool, query string, args ...any) (map[string][]SentenceBrief, error) {
	rows, err := pool.Query(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string][]SentenceBrief{}
	for rows.Next() {
		var key, text, tr string
		if err := rows.Scan(&key, &text, &tr); err != nil {
			return nil, err
		}
		if len(out[key]) >= maxSentPerWord {
			continue
		}
		out[key] = append(out[key], SentenceBrief{Text: text, Translation: tr})
	}
	return out, rows.Err()
}

// attach 把例句挂回同序的词上（ids 与 in.Words 一一对应）。
func attach(in *Input, ids []string, sents map[string][]SentenceBrief) {
	for i, id := range ids {
		in.Words[i].Sentences = sents[id]
	}
}

// Progress 为写稿进度（job 状态与前端轮询共用）。
type Progress struct {
	Seg   int    // 已完成段数（段是并行写的，不按序）
	Total int    // 总段数
	Label string // 刚写完那段的标签：开场导览 / 词 4-6 / 收尾复习
}

// segment 为一个写稿段。上下文（已讲词/已用例句/上一个词）在排段时就算好，
// 段与段之间没有数据依赖 → 可以并行写稿（42 词 16 段串行要 16 分钟，并行 3 路约 5 分钟）。
type segment struct {
	kind      string // open / words / close
	words     []WordBrief
	from      int // 本段第一个词在整集里的序号（1 起）
	doneWords []string
	doneSents []string
	prevWord  string // 上一段最后讲的那个词，用来写过渡句
}

func (s segment) label() string {
	switch s.kind {
	case "open":
		return "开场导览"
	case "close":
		return "收尾复习"
	default:
		to := s.from + len(s.words) - 1
		if s.from == to {
			return fmt.Sprintf("词 %d", s.from)
		}
		return fmt.Sprintf("词 %d-%d", s.from, to)
	}
}

// planSegments 排出整集的写稿段：开场 + 每 wordsPerSeg 词一段 + 收尾。
// 每段的上下文（已讲词/已用例句/上一个词）在这里就算好，段间无数据依赖 → 可并行写稿。
func planSegments(words []WordBrief) []segment {
	n := len(words)
	segs := make([]segment, 0, n/wordsPerSeg+2)
	segs = append(segs, segment{kind: "open"})
	var doneW, doneS []string
	prev := ""
	for i := 0; i < n; i += wordsPerSeg {
		end := min(i+wordsPerSeg, n)
		chunk := words[i:end]
		segs = append(segs, segment{
			kind: "words", words: chunk, from: i + 1,
			doneWords: append([]string(nil), doneW...),
			doneSents: append([]string(nil), doneS...),
			prevWord:  prev,
		})
		for _, w := range chunk {
			doneW = append(doneW, w.Surface)
			for _, s := range w.Sentences {
				doneS = append(doneS, s.Text)
			}
		}
		prev = chunk[len(chunk)-1].Surface
	}
	return append(segs, segment{kind: "close", doneWords: doneW, doneSents: doneS, prevWord: prev})
}

// writeConcurrency 并行写稿路数：MiniMax 未见明确 QPS 限制，3 路实测稳定，
// 再多容易撞限流反而更慢（42 词 16 段：串行 16 分钟 → 3 路约 6 分钟）。
const writeConcurrency = 3

// BuildScript 并行分段写稿、按序拼接为整集脚本；onProg 可为 nil（报告已完成段数）。
func BuildScript(ctx context.Context, in Input, onProg func(Progress)) ([]Line, error) {
	segs := planSegments(in.Words)
	type result struct {
		lines []Line
		err   error
	}
	results := make([]result, len(segs))
	var (
		mu   sync.Mutex
		done int
		sem  = make(chan struct{}, writeConcurrency)
		wg   sync.WaitGroup
	)
	for i, s := range segs {
		wg.Add(1)
		go func(i int, s segment) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			lines, err := writeSegmentRetry(ctx, in, s)
			results[i] = result{lines, err}
			if onProg != nil {
				mu.Lock()
				done++
				n := done
				mu.Unlock()
				onProg(Progress{Seg: n, Total: len(segs), Label: s.label()})
			}
		}(i, s)
	}
	wg.Wait()

	var all []Line
	for i, r := range results {
		if r.err != nil {
			return nil, fmt.Errorf("第 %d/%d 段（%s）写稿失败: %w", i+1, len(segs), segs[i].label(), r.err)
		}
		all = append(all, stitch(all, r.lines)...)
	}
	if err := ValidateScript(all); err != nil {
		return nil, err
	}
	return all, nil
}

// writeSegmentRetry 写一段，校验不过带错误原文重试（最多 4 次）。
func writeSegmentRetry(ctx context.Context, in Input, s segment) ([]Line, error) {
	var lastErr error
	for attempt := 0; attempt < 4; attempt++ {
		lines, err := writeSegment(ctx, in, s, lastErr)
		if err == nil {
			return lines, nil
		}
		lastErr = err
	}
	return nil, lastErr
}

// stitch 处理段接缝：若接上上一段会让同一 role 连排 3 行，翻转新段首行的 role。
// 两位主持都双语，翻角色不伤内容，比让 LLM 重写一段便宜得多。
func stitch(prev, next []Line) []Line {
	if len(prev) == 0 || len(next) == 0 {
		return next
	}
	run := 1
	for i := len(prev) - 2; i >= 0 && prev[i].Role == prev[len(prev)-1].Role; i-- {
		run++
	}
	if run >= 2 && next[0].Role == prev[len(prev)-1].Role {
		next[0].Role = flipRole(next[0].Role)
	}
	return next
}

func flipRole(r string) string {
	if r == RoleA {
		return RoleB
	}
	return RoleA
}

const scriptSystem = `你是日语教学播客《通勤日语电台》的脚本作者。两位主持：思思（女，role=a，中国人，日语流利）、xudong（男，role=b，日本人，中文流利）。两人都能自然地说中文和日语，任何一行由谁说都合理。
听众是通勤路上的中国日语学习者，只用耳朵听，看不到任何文字。
硬约束：
1. 只输出 JSON：{"lines":[{"role":"a","text":"..."}]}，不要任何解释文字或代码围栏。
2. 日语行必须短，不超过 16 个字符：耳朵一次只吃得下一小口，长句拆成 2—3 个片段（前半以、或，结尾）。中文行不超过 26 字，一次只说一个点，像说话不像念稿。
3. 一句话里可以中日混着说：中文部分写汉字，要念日语的词也写它的汉字（服务端会换成假名读音）。但**一行不要以日语词开头**——先垫一两个中文字（"它""这个词""比如"）再引出日语词，否则整行会被念成日语。纯日语示范行前面要有中文引子（"听——""跟我念""再来一遍"）；唯一例外是把一句日语拆成两半时的后半。
4. a/b 交替为主，同一 role 不连续超过 2 行。
5. text 里不写假名注音、不写角色名（不许出现"思思""xudong"）、不写音效或停顿提示；日语词直接写汉字（服务端会按词表转成日语读音）。
6. text 只能是中文或日语，绝不许出现英文单词或字母。
7. 口语化：可以有"嗯……""诶？""说白了""你猜怎么着"，但不要每行都塞语气词。
8. 绝不赶进度：讲透一个词比多讲一个词重要。禁止"这个词是XX意思"一句带过。
9. 节奏要像聊天，不像两篇稿子轮流朗读：每 4—6 行插一句不超过 6 字的捧哏或回应（"嗯。""对。""然后呢？""诶？""没错。"），长短行交错。`

// wordSteps 是每个词的固定讲解模板——用户反馈「像赶时间把词说完」的直接解法：
// 把"要讲什么"写死成 7 步，LLM 就没有压缩空间。
// 2026-11-04 二次反馈「中日切换不对劲，一会一个长中文一会一个长日语」：
// 日语只给一小口（≤16 字片段）、中文一次一个点（≤26 字）、插捧哏短句带节奏。
const wordSteps = `每个词写 12—18 行，按下面 7 步展开，一步都不许省：
1) 场景引入（中文 1—2 行，每行 ≤26 字）：这个词在什么真实场合会听到，给一个具体画面。
2) 首次示范：中文引子 1 行（"先听一句——"）+ 日语 ≤16 字 1 行 + 中文释义 1 行。
3) 词义与用法拆解（中文 2—3 行，每行只讲一个点：词性、搭配、和中文对应词的差别）。
4) 变形或搭配示范：中文引子 1 行 + 日语 ≤16 字 1 行 + 中文点评 1 行。
5) 易混对比（中文 1—2 行）：和哪个词容易混、什么时候用哪个。
6) 例句精讲：把例句拆成 2—3 个日语片段（每段 ≤16 字，除最后一段外都以、或，结尾），
   每个片段后面跟一行中文拆解（这个片段说的是谁/什么时候/什么动作）；整句最多再完整听一遍。
7) 记忆钩子（中文 1 行）：一句场景化联想或口语提示。
行与行之间像聊天：适时插一句 ≤6 字的回应（"嗯。""对。""然后呢？"），别让它变成轮流念稿。
严格按上面给出的词序讲，一个词讲完再讲下一个；每个词只用自己名下的例句，不要提前用后面词的例句。
词与词之间要有过渡句，不要像报清单。`

// writeSegment 拼一段的 user prompt 并调 LLM。
func writeSegment(ctx context.Context, in Input, s segment, prevErr error) ([]Line, error) {
	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("本集原料：%s，共 %d 个词。\n", in.Title, len(in.Words)))
	sb.WriteString("整集词表（只为让你知道全局，本段绝不讲解不属于你的词）：\n")
	for i, w := range in.Words {
		sb.WriteString(fmt.Sprintf("%d. %s（%s）%s\n", i+1, w.Surface, w.Reading, w.Meaning))
	}
	if len(s.doneWords) > 0 {
		sb.WriteString("\n已讲过的词（不要重复讲解，可自然提及）：" + strings.Join(s.doneWords, "、") + "\n")
	}
	if len(s.doneSents) > 0 {
		sb.WriteString("已用过的例句（不要重复）：" + strings.Join(s.doneSents, " / ") + "\n")
	}
	if s.prevWord != "" {
		sb.WriteString("\n上一段最后讲的是「" + s.prevWord + "」：本段开头用一句自然过渡承接它，但不要重复讲解它。\n")
	}

	sb.WriteString("\n")
	switch s.kind {
	case "open":
		sb.WriteString(fmt.Sprintf(`任务：写开场导览，4—6 行。
- 第一行可以是简短的日语问候（不超过 10 字，如"こんにちは。"），也可以直接中文问好。
- a 点明今天这一集来自「%s」，一共 %d 个词，并用一句话勾出这批词共同的生活场景。
- b 接话，给听众一个「听完能干什么」的预期（例如点餐、聊钱、约人不再卡壳）。
- 中文行里不要夹整句日语（要引用日语就单独成行）；开场不要讲解任何具体词义，也不要逐个报词。`, in.Title, len(in.Words)))
	case "close":
		sb.WriteString(fmt.Sprintf(`任务：写收尾复习，8—14 行。
- 从本集 %d 个词里挑最容易忘的 3—5 个做快速回顾：每个词一行日语示范 + 一行中文提示，两位主持可以抢答式一人一半过。
- 最后 a/b 各一句中文道别收尾（如"下期见"）。
- 不要引入新词，不要展开新讲解。`, len(in.Words)))
	default:
		sb.WriteString(fmt.Sprintf("任务：只讲第 %d—%d 个词，共 %d 个：\n", s.from, s.from+len(s.words)-1, len(s.words)))
		for _, w := range s.words {
			sb.WriteString(fmt.Sprintf("- %s（%s）%s\n", w.Surface, w.Reading, w.Meaning))
			for _, sn := range w.Sentences {
				sb.WriteString(fmt.Sprintf("  例句：%s / %s\n", sn.Text, sn.Translation))
			}
		}
		sb.WriteString("\n" + wordSteps)
		sb.WriteString(fmt.Sprintf("\n本段总行数控制在 %d—%d 行之间（含过渡句与捧哏短句）。",
			len(s.words)*12, len(s.words)*18))
	}
	if prevErr != nil {
		sb.WriteString("\n\n上一次输出未通过校验：" + prevErr.Error() + "。请修正后重新输出。")
	}

	content, err := chatStream(ctx, sb.String())
	if err != nil {
		return nil, err
	}
	lines, err := parseLines(content)
	if err != nil {
		return nil, err
	}
	// 模型偶发漏出英文或罗马字碎片（M3 实测 "weekdays"/"entails"，Flash 实测 "家賃atta"），
	// 整段重写要 20—90 秒且可能再漏——直接丢掉这些行划算得多；丢完若段级校验不过
	// （行数不够或某个词没了踪影）才重试。
	lines = sanitize(lines)
	// 先按行数校验「讲解有没有被压缩」，再自动拆超长行——拆开会让行数虚增，
	// 顺序反了就会把正常的段判成啰嗦。
	if err := validateSegment(s, lines); err != nil {
		return nil, err
	}
	fixed := repair(lines)
	if err := checkLines(fixed); err != nil {
		return nil, err
	}
	FillSay(fixed, in.Words) // 行首假名由 FillSay 自动垫中文引子，无需重试
	return fixed, nil
}

// sanitize 丢掉漏出拉丁字母或角色名的行。宁可少一句，也不要让主持人念出 "weekdays"。
func sanitize(lines []Line) []Line {
	out := make([]Line, 0, len(lines))
	for _, l := range lines {
		if foreignToken(l.Text) == "" {
			out = append(out, l)
		}
	}
	return out
}

// scriptModel 写稿模型：M3 关思考后会在正文里漏英文碎片（实测 "weekdays"/"entails"/"delimitate"，
// 同一段 4 次重试全败），开思考又要 5 分钟/段；M3.1-Flash 低思考档 10—30 秒/段，
// 偶发漏罗马字（"家賃atta"）由 sanitize 丢行兜住 → 定为写稿默认
// （课程生成那条线仍用 env LLM_MODEL，互不影响）。
// POD_LLM_MODEL / POD_LLM_THINK 可覆盖：think=low 低档思考 / off 关思考 / none 两个参数都不传。
const scriptModel = "MiniMax-M3.1-Flash"

// chatStream 流式收稿（长输出走流式，避免网关空闲超时）。
// 写稿默认用 scriptModel；若该模型在当前 key 上不可用（404），回退到 env LLM_MODEL 再试一次，
// 避免生产 key 权限与本地不一致时整集生成直接失败。
func chatStream(ctx context.Context, user string) (string, error) {
	first := os.Getenv("POD_LLM_MODEL")
	if first == "" {
		first = scriptModel
	}
	content, err := chatOnce(ctx, user, first, thinkMode())
	if err != nil && strings.Contains(err.Error(), "LLM 返回 404") {
		if fb := os.Getenv("LLM_MODEL"); fb != "" && fb != first {
			return chatOnce(ctx, user, fb, "off")
		}
	}
	return content, err
}

func thinkMode() string {
	if t := os.Getenv("POD_LLM_THINK"); t != "" {
		return t
	}
	return "low"
}

func chatOnce(ctx context.Context, user, model, think string) (string, error) {
	payload := map[string]any{
		"model": model,
		"messages": []map[string]any{
			{"role": "system", "content": scriptSystem},
			{"role": "user", "content": user},
		},
		"reasoning_split": true,
		"stream":          true,
	}
	switch think {
	case "low":
		payload["thinking"] = map[string]any{"type": "adaptive"}
		payload["reasoning_effort"] = "low"
	case "off":
		payload["thinking"] = map[string]any{"type": "disabled"}
	case "none":
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return "", err
	}
	target := strings.TrimSuffix(os.Getenv("LLM_BASE_URL"), "/") + "/chat/completions"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, target, bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+os.Getenv("LLM_API_KEY"))
	cli := &http.Client{Timeout: 300 * time.Second}
	resp, err := cli.Do(req)
	if err != nil {
		return "", fmt.Errorf("LLM 请求失败: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("LLM 返回 %d", resp.StatusCode)
	}
	var cb strings.Builder
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 256*1024), 4*1024*1024)
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
					Content string `json:"content"`
				} `json:"delta"`
			} `json:"choices"`
		}
		if err := json.Unmarshal([]byte(data), &chunk); err != nil || len(chunk.Choices) == 0 {
			continue
		}
		cb.WriteString(chunk.Choices[0].Delta.Content)
	}
	if err := sc.Err(); err != nil {
		return "", err
	}
	content := strings.TrimSpace(cb.String())
	if content == "" {
		return "", fmt.Errorf("LLM 无输出")
	}
	return content, nil
}

// parseLines 从模型输出里提取行级脚本。
// 关思考后模型偶发把脚本拆成多个 JSON 对象、或尾随附加说明，只取首尾花括号会得到非法 JSON
// （2026-11-04 实测 "invalid character ',' after top-level value"），因此逐个扫描平衡的
// 顶层 {...} / [...]，能解析出 lines 的就合并。
func parseLines(content string) ([]Line, error) {
	var out []Line
	for _, chunk := range topLevelJSON(content) {
		var wrap struct {
			Lines []Line `json:"lines"`
		}
		if err := json.Unmarshal([]byte(chunk), &wrap); err == nil && len(wrap.Lines) > 0 {
			out = append(out, wrap.Lines...)
			continue
		}
		var arr []Line
		if err := json.Unmarshal([]byte(chunk), &arr); err == nil && len(arr) > 0 {
			out = append(out, arr...)
		}
	}
	if len(out) == 0 {
		snip := []rune(content)
		if len(snip) > 200 {
			snip = snip[:200]
		}
		return nil, fmt.Errorf("脚本 JSON 解析失败（未找到 lines），原文开头：%s", string(snip))
	}
	return out, nil
}

// topLevelJSON 扫描出所有平衡的顶层 JSON 对象/数组，忽略字符串内的花括号与转义。
func topLevelJSON(s string) []string {
	var (
		out   []string
		depth int
		start = -1
		inStr bool
		esc   bool
	)
	for i, r := range s {
		switch {
		case inStr:
			switch {
			case esc:
				esc = false
			case r == '\\':
				esc = true
			case r == '"':
				inStr = false
			}
		case r == '"':
			inStr = true
		case r == '{' || r == '[':
			if depth == 0 {
				start = i
			}
			depth++
		case r == '}' || r == ']':
			if depth > 0 {
				depth--
				if depth == 0 && start >= 0 {
					out = append(out, s[start:i+1])
					start = -1
				}
			}
		}
	}
	return out
}

// validateSegment 段级校验：行数区间 + 本段每个词都要真的出现。
func validateSegment(s segment, lines []Line) error {
	switch s.kind {
	case "open":
		if len(lines) < 3 || len(lines) > 8 {
			return fmt.Errorf("开场行数 %d 不在 3—8", len(lines))
		}
	case "close":
		if len(lines) < 5 || len(lines) > 20 {
			return fmt.Errorf("收尾行数 %d 不在 5—20", len(lines))
		}
	default:
		if min := len(s.words) * minLinesPerWord; len(lines) < min {
			return fmt.Errorf("本段行数 %d 少于 %d（每词至少 %d 行，讲解被压缩了）", len(lines), min, minLinesPerWord)
		}
		if mx := len(s.words) * maxLinesPerWord; len(lines) > mx {
			return fmt.Errorf("本段行数 %d 超过 %d（每词最多 %d 行，讲得太啰嗦或步骤重复了）", len(lines), mx, maxLinesPerWord)
		}
		for _, w := range s.words {
			if !mentions(lines, w.Surface) {
				return fmt.Errorf("词「%s」没有在本段出现", w.Surface)
			}
		}
		jaSeen := map[string]int{}
		for _, l := range lines {
			if !isJaLine(l.Text) {
				continue
			}
			jaSeen[l.Text]++
			if jaSeen[l.Text] > maxJaRepeat {
				return fmt.Errorf("日语句「%s」在本段出现 %d 次（最多 %d 次）：重复听不如拆成片段精讲", l.Text, jaSeen[l.Text], maxJaRepeat)
			}
		}
	}
	return nil // 行级规则（长度/引子/连排/重复）在 repair 之后由 checkLines 查
}

func mentions(lines []Line, surface string) bool {
	for _, l := range lines {
		if strings.Contains(l.Text, surface) {
			return true
		}
	}
	return false
}

// checkLines 通用行规则：role 合法、非空、纯中日文、长度上限、同 role 不连排超过 2 行、长行不重复。
func checkLines(lines []Line) error {
	run, last := 0, ""
	seen := map[string]int{}
	for i, l := range lines {
		if l.Role != RoleA && l.Role != RoleB {
			return fmt.Errorf("第 %d 行 role 非法", i)
		}
		if strings.TrimSpace(l.Text) == "" {
			return fmt.Errorf("第 %d 行文本为空", i)
		}
		if bad := foreignToken(l.Text); bad != "" {
			return fmt.Errorf("第 %d 行混入非中日文内容 %q，请改写成中文或日语：%s", i, bad, l.Text)
		}
		// 行尾悬空连接词（实测出现过"山田さんは、或"这种半截行），念出来是断的。
		if tail := danglingTail(l.Text); tail != "" {
			return fmt.Errorf("第 %d 行以连接词 %q 结尾，话没说完；请把它并进下一行或改写：%s", i, tail, l.Text)
		}
		n := len([]rune(l.Text))
		if isJaLine(l.Text) {
			if n > maxJaLineRunes {
				return fmt.Errorf("第 %d 行日语过长（%d 字 > %d），拆成更短的片段：%s", i, n, maxJaLineRunes, l.Text)
			}
			// 日语行前要有中文引子，不许裸切进来（用户反馈"中日切换不对劲"的根因之一）。
			// 唯一例外：一句日语被拆成两半，前半以、或，结尾。
			if i > 0 && isJaLine(lines[i-1].Text) {
				prev := lines[i-1].Text
				if !strings.HasSuffix(prev, "、") && !strings.HasSuffix(prev, "，") {
					return fmt.Errorf("第 %d 行日语前缺中文引子（第 %d 行日语已说完），加一句\"听——\"或\"跟我念\"：%s", i, i-1, l.Text)
				}
			}
		} else {
			if n > maxZhLineRunes {
				return fmt.Errorf("第 %d 行中文过长（%d 字 > %d），一次只说一个点：%s", i, n, maxZhLineRunes, l.Text)
			}
		}
		// 中文实质行不许重复（说明模型在凑数）；日语行允许重复——"整句再听一遍"就是教学法。
		// 阈值 16 字：过渡句/引子（"来，我们把例句慢慢拆开。"）本来就会复用，
		// 真正凑数的重复是整句解释（20 字以上）。
		if n >= 16 && !isJaLine(l.Text) {
			if j, ok := seen[l.Text]; ok {
				return fmt.Errorf("第 %d 行与第 %d 行完全重复，请换一个说法：%s", i, j, l.Text)
			}
			seen[l.Text] = i
		}
		if l.Role == last {
			run++
		} else {
			run, last = 1, l.Role
		}
		if run > 2 {
			return fmt.Errorf("第 %d 行起同一 role 连排超过 2 行，请改成 a/b 交替", i)
		}
	}
	return nil
}

// foreignToken 找出行内的拉丁字母串与角色名——M3 关思考后会漏出英文碎片
// （实测出现过 "常和家族 delimitate动向"、"食費 entails全部的饭钱"），必须挡回去重写。
func foreignToken(s string) string {
	var b strings.Builder
	for _, r := range s {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') {
			b.WriteRune(r)
		} else if b.Len() > 0 {
			break
		}
	}
	if b.Len() > 0 {
		return b.String()
	}
	if strings.Contains(s, "思思") || strings.Contains(s, "xudong") {
		return "角色名"
	}
	return ""
}

// isJaLine 判定一行是否日语行：不含中文标点（，；：）、含 ≥2 个假名，
// 且不是"汉字多于假名还带「」引用"的中文句（如 日常问熟人可以用「誰ですか」。）。
// 中文标点是硬信号——日语书写不用，；：；带「」且汉字占多的是中文句引用日语，
// 纯日语即使汉字多（私は中国人です。）也不会带「」。
func isJaLine(s string) bool {
	if strings.ContainsAny(s, "，；：") {
		return false
	}
	kana, han := kanaHanCount(s)
	if kana < 2 {
		return false
	}
	if han > kana && strings.ContainsAny(s, "「」") {
		return false
	}
	return true
}

// kanaHanCount 统计一行里的假名数与汉字数。
func kanaHanCount(s string) (kana, han int) {
	for _, r := range s {
		switch {
		case (r >= 0x3040 && r <= 0x30FF) || (r >= 0xFF66 && r <= 0xFF9F):
			kana++
		case r >= 0x4E00 && r <= 0x9FFF:
			han++
		}
	}
	return kana, han
}

// danglingTail 行尾悬空的连接词——话没说完就断行，念出来是碎的。
// 只列不会作为词语结尾的字/词（"温和"以和结尾、"学校で"以で结尾，都不能误伤）。
func danglingTail(s string) string {
	s = strings.TrimSpace(s)
	for _, t := range []string{"或者", "以及", "而且", "但是", "或", "但"} {
		if strings.HasSuffix(s, t) {
			return t
		}
	}
	return ""
}

// repair 自动修掉模型最常见的几类违规，避免整段重写（一次重写 20—90 秒，16 段经不起）：
// 超长行按标点拆成同 role 连说；日语行前缺中文引子就补一行"听——"；
// 拆完/补完出现的 3 连排翻给另一位主持（两人均双语，翻角色不伤内容）。
func repair(lines []Line) []Line {
	out := make([]Line, 0, len(lines)+8)
	for _, l := range lines {
		for _, part := range splitLong(l.Text, isJaLine(l.Text)) {
			out = append(out, Line{Role: l.Role, Text: part})
		}
	}
	withCue := make([]Line, 0, len(out)+4)
	for i, l := range out {
		if i > 0 && isJaLine(l.Text) && isJaLine(out[i-1].Text) {
			prev := out[i-1].Text
			if !strings.HasSuffix(prev, "、") && !strings.HasSuffix(prev, "，") {
				withCue = append(withCue, Line{Role: flipRole(l.Role), Text: "听——"})
			}
		}
		withCue = append(withCue, l)
	}
	for i := 2; i < len(withCue); i++ {
		if withCue[i].Role == withCue[i-1].Role && withCue[i-1].Role == withCue[i-2].Role {
			withCue[i].Role = flipRole(withCue[i].Role)
		}
	}
	return withCue
}

// splitLong 在长度上限内最后一个标点处切开，递归处理剩余部分；找不到标点就原样返回，
// 交给校验报错重试（硬切会把词切断，比重试更糟）。
func splitLong(text string, ja bool) []string {
	limit := maxZhLineRunes
	if ja {
		limit = maxJaLineRunes
	}
	rs := []rune(text)
	if len(rs) <= limit {
		return []string{text}
	}
	cut := -1
	// 优先在句末标点处断，其次才是逗号顿号——断在逗号上会留下"记住，"这种半句。
	for _, marks := range []string{"。！？；", "，、,.:："} {
		for i := limit - 1; i >= 1; i-- {
			if strings.ContainsRune(marks, rs[i]) {
				cut = i + 1
				break
			}
		}
		if cut > 0 {
			break
		}
	}
	if cut <= 0 {
		return []string{text}
	}
	first := strings.TrimSpace(string(rs[:cut]))
	rest := strings.TrimSpace(string(rs[cut:]))
	if first == "" || rest == "" {
		return []string{text}
	}
	return append([]string{first}, splitLong(rest, ja)...)
}

// ValidateScript 整集校验：行数区间、日语占比、首末行中文，外加通用行规则。
func ValidateScript(lines []Line) error {
	if len(lines) < 8 {
		return fmt.Errorf("行数 %d 少于 8", len(lines))
	}
	if len(lines) > maxTotalLines {
		return fmt.Errorf("行数 %d 超过 %d", len(lines), maxTotalLines)
	}
	if err := checkLines(lines); err != nil {
		return err
	}
	ja := 0
	for _, l := range lines {
		if isJaLine(l.Text) {
			ja++
		}
	}
	if ja < 2 {
		return fmt.Errorf("日语行 %d 少于 2", ja)
	}
	if ja*2 > len(lines) {
		return fmt.Errorf("日语行占比过高（%d/%d），讲解应以中文为主", ja, len(lines))
	}
	// 首行允许一句简短日语问候（"こんにちは"），末行必须中文收尾。
	if isJaLine(lines[0].Text) && len([]rune(lines[0].Text)) > 12 {
		return fmt.Errorf("首行若是日语问候不得超过 12 字（实际 %d 字）：%s", len([]rune(lines[0].Text)), lines[0].Text)
	}
	if isJaLine(lines[len(lines)-1].Text) {
		return fmt.Errorf("末行必须为中文")
	}
	return nil
}
