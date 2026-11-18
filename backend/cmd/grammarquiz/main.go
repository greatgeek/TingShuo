// 语法题库生成器：按语法点批量出题（完形/接续/辨析/情景），两遍质检后入库并可导出 SQL。
// 题库内容不入公开仓库：生成的 SQL 写到仓库外路径，生产导入时再用 psql 应用。
//
// 用法（在 backend/ 目录，读 .env.dev 的 DATABASE_URL / LLM_*）：
//
//	go run ./cmd/grammarquiz -book 初级上 -lessons 5,6,7 -export E:/tmp/grammar/quiz-sample.sql
//	go run ./cmd/grammarquiz -book 初级上 -export E:/tmp/grammar/quiz-all.sql
package main

import (
	"context"
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode"

	"github.com/greatgeek/TingShuo/backend/internal/generate"
	"github.com/greatgeek/TingShuo/backend/pkg/envcfg"
	"github.com/greatgeek/TingShuo/backend/pkg/llmclient"
	"github.com/jackc/pgx/v5/pgxpool"
)

const blankMark = "＿＿＿"

var blankRe = regexp.MustCompile(`[_＿]{2,}`)
var kanaRe = regexp.MustCompile(`[ぁ-ゖァ-ヺー]`)

// simplifiedOnlyRe 命中只存在于简体中文、不是日文新字体的字（用于识别题干夹带的中文说明）。
var simplifiedOnlyRe = regexp.MustCompile(`[选译这说时书问题应该为变关词语陈顺并后连请让读吗呢两们对个几给见现发]`)

// junkOptionRe 拦截语料里出现过的生造/畸形选项（模型把错误选项写成不存在的日语）。
var junkOptionRe = regexp.MustCompile(`ですです|ですます|ますです|でしたです|ましたです|ませんです|[をにでねよかは]です|じゃあありません|じゃんありません|でいありません|ではんありません|じゃありませんて|ありませんで$`)

// latinJunkRe 命中成串拉丁字母（模型偶发把 Academy、Nardo 之类杂词塞进题干/解析）；
// 单字母（A：、N）不拦。
var latinJunkRe = regexp.MustCompile(`[A-Za-z]{3,}`)

type dbPoint struct {
	ID        string
	Book      string
	LessonNo  int
	Seq       int
	Section   string
	Pattern   string
	Formation string
	Body      json.RawMessage
}

type dbExample struct {
	ID          string
	PointID     string
	Seq         int
	Speaker     string
	Mark        string
	Text        string
	Translation string
	Note        string
	Ruby        json.RawMessage
	RubyPairs   [][]string
}

type genOption struct {
	Text string `json:"text"`
	Why  string `json:"why"`
}

type genQuestion struct {
	Kind      string      `json:"kind"`
	Stem      string      `json:"stem"`
	Answer    string      `json:"answer"`
	SourceSeq int         `json:"source_seq"`
	Explain   string      `json:"explain"`
	Options   []genOption `json:"options"`
}

type genResult struct {
	Questions []genQuestion `json:"questions"`
}

type storedOption struct {
	Text    string     `json:"text"`
	Ruby    [][]string `json:"ruby,omitempty"`
	Correct bool       `json:"correct"`
	Why     string     `json:"why"`
}

type readyQuestion struct {
	ID              string
	PointID         string
	PointPattern    string
	LessonNo        int
	Kind            string
	Stem            string
	StemRuby        [][]string
	Options         []storedOption
	Answer          int
	Explain         string
	SourceExampleID string
}

type verifyItem struct {
	Index      int    `json:"index"`
	WellFormed []bool `json:"wellformed"`
	Valid      []bool `json:"valid"`
	Answer     int    `json:"answer"`
	OK         bool   `json:"ok"`
	Issue      string `json:"issue"`
}

type verifyResult struct {
	Results []verifyItem `json:"results"`
}

func main() {
	var (
		book    = flag.String("book", "初级上", "教材册别")
		lessons = flag.String("lessons", "", "课号，逗号分隔；空=整册")
		limit   = flag.Int("limit", 0, "最多处理多少个知识点（0=不限，调试用）")
		workers = flag.Int("workers", 3, "并发数")
		export  = flag.String("export", "", "导出 SQL 路径（仓库外）")
		preview = flag.String("preview", "", "导出可读预览 Markdown 路径")
		exonly  = flag.Bool("export-only", false, "只导出 SQL，不调用 LLM 生成题目")
		verify  = flag.Bool("verify", true, "是否做第二遍盲答质检")
		vthink  = flag.Bool("verify-think", false, "质检时开启思考（更严格，速度较慢）")
		think   = flag.Bool("think", false, "生成时开启思考（更慢，质量更高）")
		redo    = flag.Bool("redo", false, "忽略已有题目，强制重新生成")
	)
	flag.Parse()

	_ = envcfg.Load(".env.dev", ".env")
	dsn, base, key, model := os.Getenv("DATABASE_URL"), os.Getenv("LLM_BASE_URL"), os.Getenv("LLM_API_KEY"), os.Getenv("LLM_MODEL")
	if dsn == "" || base == "" || key == "" || model == "" {
		fmt.Fprintln(os.Stderr, "缺少 DATABASE_URL / LLM_BASE_URL / LLM_API_KEY / LLM_MODEL")
		os.Exit(1)
	}
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		fmt.Fprintln(os.Stderr, "connect:", err)
		os.Exit(1)
	}
	defer pool.Close()
	llm := llmclient.New(base, key, model)

	lessonSet := map[int]bool{}
	for _, part := range strings.Split(*lessons, ",") {
		if v, err := strconv.Atoi(strings.TrimSpace(part)); err == nil && v > 0 {
			lessonSet[v] = true
		}
	}
	points, examples, err := loadGrammar(ctx, pool, *book, lessonSet)
	if err != nil {
		fmt.Fprintln(os.Stderr, "load:", err)
		os.Exit(1)
	}
	if *exonly {
		points, examples = nil, nil
	}
	have, err := questionCounts(ctx, pool)
	if err != nil {
		fmt.Fprintln(os.Stderr, "counts:", err)
		os.Exit(1)
	}
	// 全册已有题干（规范化后）用于跨知识点去重：不同知识点不能出同一句子的题
	allStems := map[string]bool{}
	stemRows, err := pool.Query(ctx, `
		SELECT q.stem FROM grammar_question q JOIN grammar_point p ON p.id = q.point_id
		WHERE p.book = $1`, *book)
	if err != nil {
		fmt.Fprintln(os.Stderr, "stems:", err)
		os.Exit(1)
	}
	for stemRows.Next() {
		var s string
		if err := stemRows.Scan(&s); err == nil {
			allStems[string(normStem(s))] = true
		}
	}
	stemRows.Close()
	var stemMu sync.Mutex

	type job struct{ p dbPoint }
	jobs := make(chan dbPoint)
	var wg sync.WaitGroup
	var done, made int64
	var mu sync.Mutex
	var previewData []readyQuestion
	start := time.Now()

	for i := 0; i < *workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for p := range jobs {
				target := 3
				if p.Section == "expression" {
					target = 2
				}
				if !*redo && int(have[p.ID]) >= target {
					atomic.AddInt64(&done, 1)
					continue
				}
				need := target - int(have[p.ID])
				if need <= 0 {
					need = target
				}
				qs, logs := generatePoint(ctx, llm, pool, p, examples[p.ID], need, *verify, *think, *vthink, allStems, &stemMu)
				for _, s := range logs {
					fmt.Println(s)
				}
				for _, q := range qs {
					if q == nil {
						continue
					}
					id := questionID(q)
					q.ID = id
					ok, err := insertQuestion(ctx, pool, q)
					if err != nil {
						fmt.Printf("  !! 入库失败 %s: %v\n", p.Pattern, err)
						continue
					}
					if ok {
						atomic.AddInt64(&made, 1)
						mu.Lock()
						previewData = append(previewData, *q)
						mu.Unlock()
					}
				}
				atomic.AddInt64(&done, 1)
				fmt.Printf("[%d/%d] %s 第%d课 #%d %s → 新增 %d 题\n",
					atomic.LoadInt64(&done), len(points), p.Book, p.LessonNo, p.Seq, p.Pattern, len(qs))
			}
		}()
	}
	processed := 0
	for _, p := range points {
		if *limit > 0 && processed >= *limit {
			break
		}
		haveN := int(have[p.ID])
		target := 3
		if p.Section == "expression" {
			target = 2
		}
		if *redo || haveN < target {
			jobs <- p
			processed++
		}
	}
	close(jobs)
	wg.Wait()

	total, _ := questionCounts(ctx, pool)
	sum := 0
	for _, v := range total {
		sum += int(v)
	}
	fmt.Printf("\n完成：处理 %d 个知识点，新增 %d 题，题库共 %d 题，用时 %s\n",
		processed, made, sum, time.Since(start).Round(time.Second))

	if *preview != "" {
		if err := writePreview(*preview, previewData); err != nil {
			fmt.Fprintln(os.Stderr, "preview:", err)
		} else {
			fmt.Println("预览已写入", *preview)
		}
	}
	if *export != "" {
		n, err := exportSQL(ctx, pool, *book, lessonSet, *export)
		if err != nil {
			fmt.Fprintln(os.Stderr, "export:", err)
			os.Exit(1)
		}
		fmt.Printf("已导出 %d 题 → %s\n", n, *export)
	}
}

func loadGrammar(ctx context.Context, pool *pgxpool.Pool, book string, lessonSet map[int]bool) ([]dbPoint, map[string][]dbExample, error) {
	rows, err := pool.Query(ctx, `
		SELECT id, book, lesson_no, seq, section, pattern, formation, body
		FROM grammar_point WHERE book = $1 ORDER BY lesson_no, seq`, book)
	if err != nil {
		return nil, nil, err
	}
	var points []dbPoint
	for rows.Next() {
		var p dbPoint
		if err := rows.Scan(&p.ID, &p.Book, &p.LessonNo, &p.Seq, &p.Section, &p.Pattern, &p.Formation, &p.Body); err != nil {
			rows.Close()
			return nil, nil, err
		}
		if len(lessonSet) > 0 && !lessonSet[p.LessonNo] {
			continue
		}
		points = append(points, p)
	}
	rows.Close()

	exRows, err := pool.Query(ctx, `
		SELECT e.id, e.point_id, e.seq, e.speaker, e.mark, e.text, e.translation, e.note,
		       COALESCE(e.ruby_tokens, '[]'::jsonb)
		FROM grammar_example e JOIN grammar_point p ON p.id = e.point_id
		WHERE p.book = $1 ORDER BY p.lesson_no, p.seq, e.seq`, book)
	if err != nil {
		return nil, nil, err
	}
	defer exRows.Close()
	examples := map[string][]dbExample{}
	for exRows.Next() {
		var e dbExample
		var ruby []byte
		if err := exRows.Scan(&e.ID, &e.PointID, &e.Seq, &e.Speaker, &e.Mark, &e.Text, &e.Translation, &e.Note, &ruby); err != nil {
			return nil, nil, err
		}
		e.Ruby = ruby
		_ = json.Unmarshal(ruby, &e.RubyPairs)
		examples[e.PointID] = append(examples[e.PointID], e)
	}
	return points, examples, exRows.Err()
}

func questionCounts(ctx context.Context, pool *pgxpool.Pool) (map[string]int, error) {
	rows, err := pool.Query(ctx, `SELECT point_id, count(*) FROM grammar_question WHERE status = 'ok' GROUP BY point_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]int{}
	for rows.Next() {
		var id string
		var n int
		if err := rows.Scan(&id, &n); err != nil {
			return nil, err
		}
		out[id] = n
	}
	return out, rows.Err()
}

func generatePoint(ctx context.Context, llm *llmclient.Client, pool *pgxpool.Pool, p dbPoint, exs []dbExample, need int, doVerify, doThink, verifyThink bool, allStems map[string]bool, stemMu *sync.Mutex) ([]*readyQuestion, []string) {
	var logs []string
	siblings, err := siblingPatterns(ctx, pool, p)
	if err != nil {
		logs = append(logs, "  !! 读取同课语法失败: "+err.Error())
	}
	ask := need + 3
	if ask > 8 {
		ask = 8
	}
	var accepted []*readyQuestion
	for attempt := 0; attempt < 2 && len(accepted) < need; attempt++ {
		prompt := buildGenPrompt(p, exs, siblings, ask)
		var content string
		var err error
		if doThink {
			content, _, _, err = llm.ChatJSON(ctx, prompt)
		} else {
			content, _, _, err = llm.ChatJSONNoThinking(ctx, prompt)
		}
		if err != nil {
			logs = append(logs, fmt.Sprintf("  !! %s 生成失败: %v", p.Pattern, err))
			time.Sleep(3 * time.Second)
			continue
		}
		var res genResult
		raw := llmclient.ExtractJSON(content)
		if err := json.Unmarshal([]byte(raw), &res); err != nil {
			logs = append(logs, fmt.Sprintf("  !! %s JSON 解析失败: %v", p.Pattern, err))
			continue
		}
		var candidates []*readyQuestion
		for _, g := range res.Questions {
			q, why := validateQuestion(p, exs, g)
			if q == nil {
				logs = append(logs, fmt.Sprintf("  -- 丢弃（%s）：%s", p.Pattern, why))
				continue
			}
			candidates = append(candidates, q)
		}
		if doVerify && len(candidates) > 0 {
			passed, vlog := verifyQuestions(ctx, llm, p, candidates, verifyThink)
			logs = append(logs, vlog...)
			candidates = passed
		}
		// 同一次生成内去重：题干高度相似或同题型超额的不收
		seen := map[string]bool{}
		for _, q := range candidates {
			if seen[q.Kind+"|"+q.Stem] {
				continue
			}
			if p.Section == "grammar" && countKind(accepted, q.Kind) >= 2 {
				continue
			}
			if nearDuplicate(accepted, q) {
				logs = append(logs, fmt.Sprintf("  -- 丢弃重复题（%s）：%s", p.Pattern, q.Stem))
				continue
			}
			// 全册题干去重：同一句子模板不得在多个知识点重复出现（同课测验会撞题）
			key := string(normStem(q.Stem))
			stemMu.Lock()
			crossDup := allStems[key]
			if !crossDup {
				allStems[key] = true
			}
			stemMu.Unlock()
			if crossDup {
				logs = append(logs, fmt.Sprintf("  -- 丢弃跨知识点重复题干（%s）：%s", p.Pattern, q.Stem))
				continue
			}
			seen[q.Kind+"|"+q.Stem] = true
			accepted = append(accepted, q)
		}
	}
	if len(accepted) > need {
		accepted = accepted[:need]
	}
	if len(accepted) < need {
		logs = append(logs, fmt.Sprintf("  ?? %s 只生成 %d/%d 题", p.Pattern, len(accepted), need))
	}
	return accepted, logs
}

func countKind(qs []*readyQuestion, kind string) int {
	n := 0
	for _, q := range qs {
		if q.Kind == kind {
			n++
		}
	}
	return n
}

// normStem 去掉标点、空白与挖空标记，只留下用于查重的字面。
func normStem(s string) []rune {
	var out []rune
	for _, r := range s {
		if unicode.IsPunct(r) || unicode.IsSpace(r) {
			continue
		}
		out = append(out, r)
	}
	return out
}

// nearDuplicate 同题型下，题干互相包含（长度相近）或二元组重合度很高即视为重复。
func nearDuplicate(qs []*readyQuestion, q *readyQuestion) bool {
	a := normStem(q.Stem)
	if len(a) == 0 {
		return false
	}
	for _, other := range qs {
		if other.Kind != q.Kind {
			continue
		}
		b := normStem(other.Stem)
		if len(b) == 0 {
			continue
		}
		short, long := a, b
		if len(short) > len(long) {
			short, long = long, short
		}
		if len(short)*10 >= len(long)*6 && strings.Contains(string(long), string(short)) {
			return true
		}
		if bigramJaccard(a, b) >= 0.75 {
			return true
		}
	}
	return false
}

func bigramJaccard(a, b []rune) float64 {
	if len(a) < 2 || len(b) < 2 {
		return 0
	}
	set := func(rs []rune) map[string]bool {
		m := make(map[string]bool, len(rs))
		for i := 0; i+1 < len(rs); i++ {
			m[string(rs[i:i+2])] = true
		}
		return m
	}
	sa, sb := set(a), set(b)
	inter := 0
	for k := range sa {
		if sb[k] {
			inter++
		}
	}
	union := len(sa) + len(sb) - inter
	if union == 0 {
		return 0
	}
	return float64(inter) / float64(union)
}

func siblingPatterns(ctx context.Context, pool *pgxpool.Pool, p dbPoint) ([]string, error) {
	rows, err := pool.Query(ctx, `
		SELECT lesson_no, seq, pattern FROM grammar_point
		WHERE book = $1 AND lesson_no <= $2 AND id <> $3 AND section = 'grammar'
		ORDER BY lesson_no DESC, seq DESC LIMIT 24`, p.Book, p.LessonNo, p.ID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var no, seq int
		var pat string
		if err := rows.Scan(&no, &seq, &pat); err == nil {
			out = append(out, fmt.Sprintf("第%d课：%s", no, pat))
		}
	}
	return out, rows.Err()
}

func buildGenPrompt(p dbPoint, exs []dbExample, siblings []string, ask int) string {
	var b strings.Builder
	fmt.Fprintf(&b, `你是日语教材《新编日语》初级（面向中国学习者）的命题老师。请为下面这个知识点出 %d 道四选一题，用于语法本测验。

教材：%s 第%d课
知识点：%s
类型：%s
`, ask, p.Book, p.LessonNo, p.Pattern, sectionLabel(p.Section))
	if p.Formation != "" {
		fmt.Fprintf(&b, "接续：%s\n", p.Formation)
	}
	if txt := bodyText(p.Body); txt != "" {
		fmt.Fprintf(&b, "讲解摘要：\n%s\n", truncRunes(txt, 1600))
	}
	if len(siblings) > 0 {
		fmt.Fprintf(&b, "学员已学的其他语法（干扰项只能从这里选，或用本知识点的错误变形）：\n- %s\n", strings.Join(siblings, "\n- "))
	}
	if len(exs) > 0 {
		fmt.Fprintf(&b, "本知识点参考例句（原文不得改写；cloze 若用原句必须逐字一致）：\n")
		for i, e := range exs {
			if i >= 20 {
				break
			}
			label := ""
			if e.Speaker != "" {
				label = e.Speaker + "："
			}
			if e.Mark != "" {
				label += e.Mark + " "
			}
			fmt.Fprintf(&b, "[seq %d] %s%s（%s）\n", e.Seq, label, e.Text, e.Translation)
		}
	} else {
		b.WriteString("本知识点没有现成例句，请自行编写自然、简短的例句（只用本课及之前学过的词汇与语法）。\n")
	}
	b.WriteString(`
出题总原则（必须遵守）：
A. 题干语境必须足以唯一确定答案：用时间词、否定回答、问答对话、前后分句补足线索。禁止信息不足的题干，例如「私は＿＿＿。」「山田さんは＿＿＿。」「＿＿＿です。」（任何名词都能填）；题干不要以空格开头。
   题干核心必须是一句/一段自然的日语（可带简短中文说明），空格嵌在日语句子内部；禁止模板碎片、词表拼接（如「初めに＿＿＿＋ね。」）或中日文脱节的半截碎片（如「「学生」の＿＿＿は「部」です。」）。
   禁止引用教材名称或课号（如「新版初级上第六课中……」）；禁止在题干或示例里直接写出答案（如问「に」后接什么，示例却已写「には」）；禁止「在…句型中助词X后应接＿＿＿」这类元语言问法。
   题干、解析、选项里不得夹带英语单词、罗马字或拼音人名（Academy、Nardo、KER、Zhang 这类杂词一律禁止）。
B. 四个选项都必须是真实、规范的日语：每个选项单独拿出来，都要能作为某个自然句子中的正确形式。干扰项只能靠「在本句语境下不成立」来区分：错助词（は・が・を・に・で）、错活用形（行きます・行きません・行きました・行きませんでした）、错时态、错肯定否定、近义表达不合场景。
   严禁生造不存在的日语：ではありませんで、じゃあありません、じゃんありません、でいありません、学生ですです、山田をです、はですか 这类写法一律禁止。
C. 反例（禁止）：私は＿＿＿。（山田です・山田でした・山田じゃありません 都能填，语境不足）；干扰项写成 山田をです・山田かもです 这类生造形式。
   正例（助词）：A：田中さんは学生ですか。 B：いいえ、田中さんは学生＿＿＿。会社員です。／ではありません
   正例（变形）：明日は日曜日ですから、学校へ＿＿＿。／行きません（干扰项用 行きます・行きました・行きませんでした 等真实活用形，靠「明日」排除）
   允许用「改成疑问句／否定句」这类变换要求补足线索，但不能靠「选择基本形式」这种语文描述把题目变成「挑唯一没写错的选项」。
D. cloze 的 answer 只能是目标语法点本身（助词、助动词、词尾、句型形式），只含假名、不能含汉字，不能是名词、人名或动词词干；题干只挖这一处。
   从参考例句出题时必须逐字一致；原句线索不足或目标不是语法点时，改成带上下文的自编题并令 source_seq=0。
E. 所有日语选项必须含假名，不要出现纯汉字选项或中文选项。
F. 每道题必须直接考查本知识点：空格挖在本知识点的形式位置上（助词、句型、活用词尾），题干其余部分与选项都围绕它；不得出成别的语法点（例：知识点是「から」，却把动词活用挖空）。
   干扰项要选代入后语法不成立或语义明显不合的形式；不要放仅语气略有差别的同义连接词（けれど、のに、なので 等），否则一题多解。
   题干不得自我泄题（如给出「食べる」再问辞书形）；不得用词表、箭头拼接；同一册内不要复用其他知识点的句子模板（如「明日は日曜日ですから、学校へ＿＿＿。」这类固定句不得反复套用）。

题型要求：
`)
	if p.Section == "grammar" {
		b.WriteString(`1. 有参考例句时，至少 1 道 cloze、1 道 form、1 道 discriminate；没有合适的易混形式时，discriminate 可换成另一道带线索的 cloze。
   没有参考例句时，用 form 与 discriminate 覆盖，题干自编。
   cloze 题：从参考例句挖空（source_seq 填该例句序号）或自编带线索的句子（source_seq=0），题干中目标处写成 ＿＿＿，answer 是被挖掉的目标语法点（只含假名），且必须与正确选项的 text 完全一致。
   form 题：考接续或变形，题干含 ＿＿＿，例如「＿＿＿＋ながら」或给动词原形让选正确接续；四个选项必须是真实活用形或真实接续形式，不能生造。
   discriminate 题：与本课或之前学过的易混形式对比，题干含 ＿＿＿，四个选项都是已学形式，只有一项在语境中成立。
`)
	} else {
		b.WriteString(`1. 这是「表达扩展」知识点，不是语法规则。根据内容出题：
   - 寒暄、社交、口语表达：kind=expression，题干用中文写一个具体场景（可以没有挖空），四个日语选项，只有一个最得体。场景必须自洽，不能出现自相矛盾的行动或人物关系。
   - 数字、日期、时刻、数量词、读音规则：kind=form，题干如「“4月8日”读作：＿＿＿」，选项为假名读法。
   - 若内容本身是语法性说明且给了例句，也可以出 cloze/discriminate（要求同语法题）。
`)
	}
	b.WriteString(`2. 每题必须恰好 4 个选项且只有 1 个正确；选项不能重复，四个选项都必须是真实日语（不能生造）。
3. answer 必须与正确选项的 text 完全一致（cloze 从原句挖空时，answer 还必须能在原例句中逐字找到，且整句只出现一次）。
4. 每个选项给 why（中文，30 字内，说明对/错在哪）。
5. 日语必须自然、无语法错误；题干与选项不要用罗马字；不要考查超出本课范围的词汇和语法。
6. explain 用中文写 60 字内的考点总结。

只输出 JSON，不要任何解释或 Markdown 围栏：
{"questions":[{"kind":"cloze","stem":"……＿＿＿……","answer":"……","source_seq":1,"explain":"……","options":[{"text":"……","why":"……"},{"text":"……","why":"……"},{"text":"……","why":"……"},{"text":"……","why":"……"}]}]}
`)
	return b.String()
}

func sectionLabel(s string) string {
	if s == "expression" {
		return "表达扩展"
	}
	return "语法解释"
}

func bodyText(raw json.RawMessage) string {
	var blocks []map[string]any
	if err := json.Unmarshal(raw, &blocks); err != nil {
		return ""
	}
	var b strings.Builder
	cell := func(v any) string {
		switch t := v.(type) {
		case string:
			return t
		case map[string]any:
			if s, ok := t["t"].(string); ok {
				return s
			}
		}
		return ""
	}
	for _, blk := range blocks {
		switch blk["type"] {
		case "p":
			if s, _ := blk["text"].(string); s != "" {
				b.WriteString(s + "\n")
			}
		case "item":
			term, _ := blk["term"].(string)
			text, _ := blk["text"].(string)
			b.WriteString("・" + term + "：" + text + "\n")
		case "ex":
			// 例句单独从 grammar_example 读，正文不重复
		case "table":
			if title, _ := blk["title"].(string); title != "" {
				b.WriteString("〔" + title + "〕\n")
			}
			if head, ok := blk["head"].([]any); ok && len(head) > 0 {
				var cells []string
				for _, h := range head {
					cells = append(cells, cell(h))
				}
				b.WriteString("| " + strings.Join(cells, " | ") + " |\n")
			}
			if rows, ok := blk["rows"].([]any); ok {
				for _, r := range rows {
					if arr, ok := r.([]any); ok {
						var cells []string
						for _, c := range arr {
							cells = append(cells, cell(c))
						}
						b.WriteString("| " + strings.Join(cells, " | ") + " |\n")
					}
				}
			}
		}
	}
	return strings.TrimSpace(b.String())
}

func truncRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}

func validateQuestion(p dbPoint, exs []dbExample, g genQuestion) (*readyQuestion, string) {
	g.Kind = strings.TrimSpace(g.Kind)
	switch g.Kind {
	case "cloze", "form", "discriminate", "expression":
	default:
		return nil, "未知题型 " + g.Kind
	}
	answer := strings.TrimSpace(g.Answer)
	if answer == "" {
		return nil, "answer 为空"
	}
	var opts []genOption
	for _, o := range g.Options {
		o.Text = strings.TrimSpace(o.Text)
		o.Why = strings.TrimSpace(o.Why)
		if o.Text == "" {
			return nil, "存在空选项"
		}
		if !kanaRe.MatchString(o.Text) {
			return nil, "选项不含假名（疑似中文或纯汉字）：" + o.Text
		}
		if junkOptionRe.MatchString(o.Text) {
			return nil, "选项疑似生造（畸形助词/活用组合）：" + o.Text
		}
		// 表达情景题的选项是整句回答，句末「。」属正常书写；其余题型靠句读凑干扰项一律拦下
		if strings.HasSuffix(o.Text, "、") || strings.HasSuffix(o.Text, "，") ||
			(strings.HasSuffix(o.Text, "。") && g.Kind != "expression" && p.Section != "expression") {
			return nil, "选项带句读符号（靠标点凑干扰项）：" + o.Text
		}
		opts = append(opts, o)
	}
	if len(opts) != 4 {
		return nil, fmt.Sprintf("选项数=%d", len(opts))
	}
	seen := map[string]bool{}
	for _, o := range opts {
		if seen[o.Text] {
			return nil, "选项重复：" + o.Text
		}
		seen[o.Text] = true
	}
	answerIdx := -1
	for i, o := range opts {
		if o.Text == answer {
			answerIdx = i
		}
	}
	if answerIdx < 0 {
		return nil, "answer 与任何选项都不一致"
	}

	q := &readyQuestion{
		PointID:      p.ID,
		PointPattern: p.Pattern,
		LessonNo:     p.LessonNo,
		Kind:         g.Kind,
		Explain:      strings.TrimSpace(g.Explain),
		Answer:       answerIdx,
		Stem:         normalizeBlank(g.Stem),
	}
	if g.Kind != "expression" && strings.Contains(q.Stem, "＋") {
		return nil, "题干含模板符号＋（词表拼接，不是自然句子）：" + q.Stem
	}
	if strings.Contains(q.Stem, "→") {
		return nil, "题干含箭头（词表拼接，不是自然句子）：" + q.Stem
	}
	if strings.Contains(q.Stem, "选项") || strings.Contains(q.Stem, "選項") {
		return nil, "题干描述了选项本身（泄题/元语言）：" + q.Stem
	}
	for _, o := range opts {
		q.Options = append(q.Options, storedOption{
			Text: o.Text, Ruby: rubyOf(o.Text), Correct: o.Text == answer, Why: o.Why,
		})
	}

	switch g.Kind {
	case "cloze":
		// 完形挖掉的应是语法形式的变形部分：纯汉字答案说明挖掉了名词/词干；
		// 含汉字但也含假名的活用形（難しくない、寒くて、食べます）是正常考点。
		if !kanaRe.MatchString(answer) {
			return nil, "cloze 答案不含假名（疑似挖掉名词）：" + answer
		}
		if g.SourceSeq > 0 {
			var src *dbExample
			for i := range exs {
				if exs[i].Seq == g.SourceSeq {
					src = &exs[i]
					break
				}
			}
			if src == nil {
				return nil, fmt.Sprintf("source_seq=%d 不存在", g.SourceSeq)
			}
			if strings.Count(src.Text, answer) != 1 {
				return nil, "答案在原句中不是唯一子串"
			}
			q.Stem = strings.Replace(src.Text, answer, blankMark, 1)
			q.SourceExampleID = src.ID
			q.StemRuby = cutRuby(src.RubyPairs, src.Text, answer)
		} else if strings.Count(q.Stem, blankMark) != 1 {
			return nil, "题干挖空数不为 1"
		}
	case "form", "discriminate":
		if strings.Count(q.Stem, blankMark) != 1 {
			return nil, "题干挖空数不为 1"
		}
	case "expression":
		// 情景题干可以没有挖空
	}
	if g.Kind != "expression" && p.Section != "expression" && !kanaRe.MatchString(q.Stem) {
		return nil, "题干不含假名（疑似中文）：" + q.Stem
	}
	if g.Kind != "expression" && p.Section != "expression" {
		if strings.HasPrefix(strings.TrimSpace(q.Stem), blankMark) {
			return nil, "题干以空格开头（语境不足）：" + q.Stem
		}
		// 语法题题干应是纯日语句子；出现简体中文专用字说明中文混进了句子（如「窓は现在…」）
		if simplifiedOnlyRe.MatchString(q.Stem) {
			return nil, "题干夹带简体中文（语法题须纯日语）：" + q.Stem
		}
		stemText := blankRe.ReplaceAllString(q.Stem, "")
		core := strings.Trim(stemText, " 　，。？！、,.?!\r\n\t")
		if len([]rune(core)) < 5 {
			return nil, "题干语境过短（有效文字不足 5 个）：" + q.Stem
		}
	}
	if latinJunkRe.MatchString(q.Stem) || latinJunkRe.MatchString(q.Explain) {
		return nil, "题干或解析夹带杂词（成串拉丁字母）：" + q.Stem
	}
	if len(q.StemRuby) == 0 && g.Kind != "expression" && !hasSimplifiedChinese(q.Stem) {
		q.StemRuby = rubyOf(q.Stem)
	}
	return q, ""
}

// hasSimplifiedChinese 判断题干是否夹带简体中文说明：这些字不是日文新字体，
// 出现即说明题干混入中文。混排题干不生成逐字注音（分词器会给中文按日语音读硬注音）。
func hasSimplifiedChinese(s string) bool {
	return simplifiedOnlyRe.MatchString(s)
}

func normalizeBlank(s string) string {
	return blankRe.ReplaceAllString(s, blankMark)
}

// rubyOf 用课程同款分词器生成振り仮名；无法严格覆盖原文时退回纯文本。
func rubyOf(text string) [][]string {
	toks := generate.BuildRubyTokens(text)
	out := make([][]string, 0, len(toks))
	var b strings.Builder
	for _, t := range toks {
		if len(t) < 1 {
			continue
		}
		s, _ := t[0].(string)
		r := ""
		if len(t) > 1 && t[1] != nil {
			r, _ = t[1].(string)
		}
		out = append(out, []string{s, r})
		b.WriteString(s)
	}
	if b.String() != text {
		return nil
	}
	return out
}

// cutRuby 从例句的 ruby_tokens 中切出挖空后的题干 token；边界无法对齐时返回 nil（纯文本渲染）。
func cutRuby(tokens [][]string, source, answer string) [][]string {
	start := strings.Index(source, answer)
	if start < 0 {
		return nil
	}
	end := start + len(answer)
	pos := 0
	var out [][]string
	inserted := false
	add := func(w, r string) {
		if w != "" {
			out = append(out, []string{w, r})
		}
	}
	for _, t := range tokens {
		if len(t) < 2 {
			return nil
		}
		w, r := t[0], t[1]
		ts, te := pos, pos+len(w)
		pos = te
		switch {
		case te <= start:
			add(w, r)
		case ts >= end:
			if !inserted {
				add(blankMark, "")
				inserted = true
			}
			add(w, r)
		default:
			if ts < start {
				add(w[:start-ts], "")
			}
			if !inserted {
				add(blankMark, "")
				inserted = true
			}
			if te > end {
				add(w[end-ts:], "")
			}
		}
	}
	if !inserted {
		add(blankMark, "")
	}
	var b strings.Builder
	for _, t := range out {
		b.WriteString(t[0])
	}
	if b.String() != strings.Replace(source, answer, blankMark, 1) {
		return nil
	}
	return out
}

func verifyQuestions(ctx context.Context, llm *llmclient.Client, p dbPoint, qs []*readyQuestion, doThink bool) ([]*readyQuestion, []string) {
	var b strings.Builder
	fmt.Fprintf(&b, "本组题对应的知识点：%s\n", p.Pattern)
	if p.Formation != "" {
		fmt.Fprintf(&b, "接续：%s\n", p.Formation)
	}
	b.WriteString(`下面是一组日语四选一题。请逐题、逐项检验，不要揣测出题人意图，也不要为了让题目成立而脑补题干没有的信息。
对每题：
1) 先看题干是否合格：完形/接续/辨析题的核心必须是一句/一段自然的日语（可带简短中文说明），空格嵌在日语句子内部；模板碎片（如「初めに＿＿＿＋ね。」）、词表拼接（含用「→」连词表的）、中日文脱节、日语本身不通的，题干不合格。中文说明必须是独立完整的说明句，不能嵌进日语句子中间（如「窓は现在仍然破着。」这类中日混排整句不合格）。题干以空格开头、或除空格外几乎没有语境（如「＿＿＿です。」）、题干/解析夹带英语单词或拼音人名（Academy、Nardo、KER、Zhang 之类）、示例里已经写出答案（问「に」后接什么，示例却已写「には」）、引用教材名称课号的元语言题，均按题干不合格处理。expression 情景题例外：题干可以只有中文场景说明（如「分别时想向老师表达感谢，最得体的是哪一句？」），不要求含日语核心句，只要场景清楚、空格位置明确即可；但场景自相矛盾（如「客人留下点心，你递给对方礼物」）算不合格。
2) 再看四个选项本身是否是真实、规范的日语形式：生造词、畸形助词组合、不存在的活用形、末尾带句读符号靠标点凑数的，都算不规范（表达情景题整句回答末尾的「。」属正常书写，不算），wellformed 为 4 个布尔值，必须全部为 true。
3) 把每个选项分别代入题干空格（expression 题：判断作为该场景的回答是否得体），判断是否在语法与语境上成立，得到 valid（4 个布尔值）。语境不成立但本身规范真实的干扰项，valid=false。注意近义连接词（が／けれど／ですが／のに 等）在同一语境可能都成立，不要因为出题人标了某个答案就硬判唯一。
4) 再看每题是否真的考查了上方知识点：空格应落在该知识点的形式位置（助词、句型、活用词尾），选项围绕该知识点；若空格考的是别的语法（知识点是「から」，空格却挖在动词上）、题干自泄答案（问辞书形却已写出辞书形）、解析与知识点无关，按题干不合格处理。
5) 最后核对解析与选项说明：explain（解析）和每个选项的 why 必须与正确答案一致且本身正确。若解析把正确项说成错误、与答案矛盾、或选项说明声称某个错误项「正确」，按不合格处理，issue 指出是哪一条。expression 题的选项说明可以只讲得体性差异。
6) 只有题干合格、wellformed 全为 true 且恰好 1 个选项在语境中成立时，answer 填该下标、ok=true；若有 0 个或 2 个以上成立、存在生造选项、题干不合格、解析自相矛盾，ok=false，并在 issue 用中文简短说明。
题干信息不足、多个选项都能说得通时，必须如实标成多个 true。

`)
	for i, q := range qs {
		fmt.Fprintf(&b, "[%d] 题型：%s\n题干：%s\n", i, q.Kind, q.Stem)
		for j, o := range q.Options {
			fmt.Fprintf(&b, "  %d. %s（选项说明：%s）\n", j, o.Text, o.Why)
		}
		if q.Explain != "" {
			fmt.Fprintf(&b, "  解析：%s\n", q.Explain)
		}
		b.WriteString("\n")
	}
	b.WriteString(`返回 JSON：
{"results":[{"index":0,"wellformed":[true,true,true,true],"valid":[false,true,false,false],"answer":1,"ok":true,"issue":""}]}
`)
	var content string
	var err error
	if doThink {
		content, _, _, err = llm.ChatJSON(ctx, b.String())
	} else {
		content, _, _, err = llm.ChatJSONNoThinking(ctx, b.String())
	}
	if err != nil {
		return nil, []string{"  !! 质检请求失败，整组丢弃: " + err.Error()}
	}
	var res verifyResult
	if err := json.Unmarshal([]byte(llmclient.ExtractJSON(content)), &res); err != nil {
		return nil, []string{"  !! 质检 JSON 解析失败，整组丢弃: " + err.Error()}
	}
	byIdx := map[int]verifyItem{}
	for _, it := range res.Results {
		byIdx[it.Index] = it
	}
	var passed []*readyQuestion
	var logs []string
	for i, q := range qs {
		it, ok := byIdx[i]
		if !ok {
			logs = append(logs, fmt.Sprintf("  -- 质检缺题（%s）: %s", p.Pattern, q.Stem))
			continue
		}
		if len(it.WellFormed) != 4 {
			logs = append(logs, fmt.Sprintf("  -- 质检缺少选项规范性（%s）: %s", p.Pattern, q.Stem))
			continue
		}
		junk := false
		for _, wf := range it.WellFormed {
			if !wf {
				junk = true
			}
		}
		if junk {
			logs = append(logs, fmt.Sprintf("  -- 质检发现生造选项（%s）: %s", p.Pattern, q.Stem))
			continue
		}
		validCount := 0
		for _, v := range it.Valid {
			if v {
				validCount++
			}
		}
		if len(it.Valid) != 4 || validCount != 1 {
			logs = append(logs, fmt.Sprintf("  -- 质检不唯一（%s）: 成立%d项｜%s", p.Pattern, validCount, q.Stem))
			continue
		}
		if !it.Valid[q.Answer] {
			logs = append(logs, fmt.Sprintf("  -- 质检否定答案（%s）: %s", p.Pattern, q.Stem))
			continue
		}
		if !it.OK {
			logs = append(logs, fmt.Sprintf("  -- 质检不通过（%s）: %s", p.Pattern, it.Issue))
			continue
		}
		if it.Answer != q.Answer {
			logs = append(logs, fmt.Sprintf("  -- 质检答案不一致（%s）: 模型选%d，答案%d｜%s", p.Pattern, it.Answer, q.Answer, q.Stem))
			continue
		}
		passed = append(passed, q)
	}
	return passed, logs
}

func questionID(q *readyQuestion) string {
	h := sha1.Sum([]byte(q.PointID + "|" + q.Kind + "|" + q.Stem))
	return "gq_" + hex.EncodeToString(h[:])[:20]
}

func insertQuestion(ctx context.Context, pool *pgxpool.Pool, q *readyQuestion) (bool, error) {
	ruby, _ := json.Marshal(q.StemRuby)
	if len(q.StemRuby) == 0 {
		ruby = []byte("[]")
	}
	opts, _ := json.Marshal(q.Options)
	var src any
	if q.SourceExampleID != "" {
		src = q.SourceExampleID
	}
	tag, err := pool.Exec(ctx, `
		INSERT INTO grammar_question (id, point_id, kind, stem, stem_ruby, options, explain, source_example_id, status)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, 'ok')
		ON CONFLICT DO NOTHING`,
		q.ID, q.PointID, q.Kind, q.Stem, string(ruby), string(opts), q.Explain, src)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() > 0, nil
}

func writePreview(path string, qs []readyQuestion) error {
	var b strings.Builder
	b.WriteString("# 语法题预览\n\n")
	for _, q := range qs {
		fmt.Fprintf(&b, "## [%s] 第%d课 · %s\n\n题干：%s\n\n", q.Kind, q.LessonNo, q.PointPattern, q.Stem)
		for i, o := range q.Options {
			mark := "　"
			if i == q.Answer {
				mark = "✓"
			}
			fmt.Fprintf(&b, "%s %d. %s — %s\n", mark, i+1, o.Text, o.Why)
		}
		fmt.Fprintf(&b, "\n解析：%s\n\n", q.Explain)
	}
	return os.WriteFile(path, []byte(b.String()), 0o644)
}

func exportSQL(ctx context.Context, pool *pgxpool.Pool, book string, lessonSet map[int]bool, path string) (int, error) {
	rows, err := pool.Query(ctx, `
		SELECT q.id, q.point_id, q.kind, q.stem, q.stem_ruby, q.options, q.explain,
		       COALESCE(q.source_example_id, ''), q.status, p.lesson_no
		FROM grammar_question q JOIN grammar_point p ON p.id = q.point_id
		WHERE p.book = $1 AND q.status = 'ok'
		ORDER BY p.lesson_no, p.seq, q.kind, q.id`, book)
	if err != nil {
		return 0, err
	}
	defer rows.Close()
	var vals []string
	for rows.Next() {
		var id, pid, kind, stem, ruby, opts, explain, src, status string
		var lessonNo int
		if err := rows.Scan(&id, &pid, &kind, &stem, &ruby, &opts, &explain, &src, &status, &lessonNo); err != nil {
			return 0, err
		}
		if len(lessonSet) > 0 && !lessonSet[lessonNo] {
			continue
		}
		srcSQL := "NULL"
		if src != "" {
			srcSQL = sqlStr(src)
		}
		vals = append(vals, fmt.Sprintf("(%s, %s, %s, %s, %s::jsonb, %s::jsonb, %s, %s, %s)",
			sqlStr(id), sqlStr(pid), sqlStr(kind), sqlStr(stem), sqlStr(ruby), sqlStr(opts), sqlStr(explain), srcSQL, sqlStr(status)))
	}
	if err := rows.Err(); err != nil {
		return 0, err
	}
	var b strings.Builder
	b.WriteString("BEGIN;\n")
	for i := 0; i < len(vals); i += 50 {
		end := i + 50
		if end > len(vals) {
			end = len(vals)
		}
		b.WriteString("INSERT INTO grammar_question (id, point_id, kind, stem, stem_ruby, options, explain, source_example_id, status) VALUES\n")
		b.WriteString(strings.Join(vals[i:end], ",\n"))
		b.WriteString("\nON CONFLICT DO NOTHING;\n")
	}
	b.WriteString("COMMIT;\n")
	if err := os.WriteFile(path, []byte(b.String()), 0o644); err != nil {
		return 0, err
	}
	return len(vals), nil
}

func sqlStr(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "''") + "'"
}
