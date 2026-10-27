// 全链路冒烟（部署文档 §6.2）：BYOK 后本脚本模拟「客户端」角色——
// 以 .env.dev 的 Key 直连 LLM 生成材料、直连 Azure 生成音频，走完整服务端闭环。
// 在 backend/ 目录运行：go run ./cmd/smoke（需 server 已启动、PG/RustFS 可达）。
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/greatgeek/TingShuo/backend/internal/generate"
	"github.com/greatgeek/TingShuo/backend/pkg/envcfg"
	"github.com/greatgeek/TingShuo/backend/pkg/llmclient"
	"github.com/greatgeek/TingShuo/backend/pkg/storage"
	"github.com/greatgeek/TingShuo/backend/pkg/ttsclient"
)

var (
	base   string
	client = &http.Client{Timeout: 300 * time.Second}
	token  string

	stageStart time.Time
	llmIn, llmOut, ttsChars int
)

func step(name string) { stageStart = time.Now(); fmt.Printf("\n== %s ==\n", name) }
func done(extra ...any) {
	fmt.Printf("   完成（%.1fs）", time.Since(stageStart).Seconds())
	if len(extra) > 0 {
		fmt.Print(" · ", fmt.Sprint(extra...))
	}
	fmt.Println()
}

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "\n冒烟失败:", err)
		os.Exit(1)
	}
}

func run() error {
	_ = envcfg.Load(".env.dev", ".env")
	var err error
	base, err = envcfg.MustGet("PUBLIC_BASE_URL")
	if err != nil {
		return err
	}
	base = strings.TrimSuffix(base, "/")
	llmKey, _ := envcfg.MustGet("LLM_API_KEY")
	llmURL, _ := envcfg.MustGet("LLM_BASE_URL")
	llmModel, _ := envcfg.MustGet("LLM_MODEL")
	azureKey, _ := envcfg.MustGet("AZURE_SPEECH_KEY")
	azureRegion, _ := envcfg.MustGet("AZURE_SPEECH_REGION")

	llm := llmclient.New(llmURL, llmKey, llmModel)
	azure := ttsclient.New(azureKey, azureRegion)
	store, err := storage.New(
		mustEnv("S3_ENDPOINT"), mustEnv("S3_ACCESS_KEY"), mustEnv("S3_SECRET_KEY"),
		mustEnv("S3_BUCKET_AUDIO"), mustEnv("S3_BUCKET_RECORDING"))
	if err != nil {
		return err
	}

	total := time.Now()

	// ---- 1. 注册 ----
	step("1 注册测试账号")
	email := fmt.Sprintf("smoke-%d@test.dev", time.Now().Unix())
	regReq, _ := json.Marshal(map[string]string{
		"email": email, "password": "smoke-pass-123", "nickname": "冒烟"})
	regResp, err := post("/api/v1/auth/register", regReq, false)
	if err != nil {
		return err
	}
	token = regResp["access_token"].(string)
	done("user:", email)

	// ---- 2. 提交词单 ----
	step("2 提交 20 词词单")
	words := smokeWords()
	lessonBody, _ := json.Marshal(map[string]any{
		"lesson_date": time.Now().Format("2006-01-02"),
		"words":       words,
		"params":      map[string]any{"scenes": []string{"便利店", "车站", "家"}, "level": "N5"},
	})
	lessonResp, err := post("/api/v1/lessons", lessonBody, true)
	if err != nil {
		return err
	}
	lessonID := lessonResp["lesson_id"].(string)
	done("lesson:", lessonID)

	// ---- 3. LLM 生成材料（客户端角色，模拟真实 App）----
	step("3 直连 LLM 生成材料")
	prompt := buildPrompt(words)
	var out generate.GenerationOutput
	var raw string
	for attempt := 1; attempt <= 2; attempt++ { // AC-2.3：schema 不通过自动重试一次
		content, in, outn, err := llm.ChatJSON(context.Background(), prompt)
		if err != nil {
			return fmt.Errorf("LLM 调用失败: %w", err)
		}
		llmIn += in
		llmOut += outn
		raw = llmclient.ExtractJSON(content)
		if err := json.Unmarshal([]byte(raw), &out); err != nil {
			if attempt == 2 {
				return fmt.Errorf("材料 JSON 解析失败（已重试）: %w …%.200s", err, raw)
			}
			continue
		}
		// 本地预校验（与服务端同规则：完整包 strict）
		if problems := out.Validate(wordKeyMap(words), true); len(problems) > 0 {
			if attempt == 2 {
				return fmt.Errorf("材料校验不通过（已重试）: %v", problems)
			}
			fmt.Println("   校验未通过，重试一次:", problems)
			continue
		}
		break
	}
	done(fmt.Sprintf("LLM in=%d out=%d tokens，材料 %d 段", llmIn, llmOut, len(out.Materials)))

	// word_occurrence key：surface → 服务端 word_id 重映射
	full, err := get("/api/v1/lessons/" + lessonID)
	if err != nil {
		return err
	}
	wordIDBySurface := map[string]string{}
	for _, w := range full["words"].([]any) {
		m := w.(map[string]any)
		wordIDBySurface[m["surface"].(string)] = m["id"].(string)
	}
	newOcc := map[string]generate.WordOcc{}
	for k, v := range out.WordOccurrence {
		if id, ok := wordIDBySurface[k]; ok {
			newOcc[id] = v
		} else {
			newOcc[k] = v
		}
	}
	out.WordOccurrence = newOcc

	// ---- 4. 上传材料（服务端校验 + kagome 振り仮名增强 + 落库）----
	step("4 上传材料（schema 校验 + kagome 增强）")
	matBody, _ := json.Marshal(out)
	matResp, err := post("/api/v1/lessons/"+lessonID+"/materials", matBody, true)
	if err != nil {
		return err
	}
	if len(matResp["warnings"].([]any)) > 0 {
		fmt.Println("   warnings:", matResp["warnings"])
	}
	done("status:", matResp["status"])

	// ---- 5. 拉全量、收集句子 ----
	step("5 拉取课程全量")
	full, err = get("/api/v1/lessons/" + lessonID)
	if err != nil {
		return err
	}
	type sent struct{ ID, Text string }
	var sents []sent
	for _, m := range full["materials"].([]any) {
		for _, s := range m.(map[string]any)["sentences"].([]any) {
			sm := s.(map[string]any)
			sents = append(sents, sent{sm["id"].(string), sm["text"].(string)})
		}
	}
	done("句子总数:", len(sents))

	// ---- 6. audio/check 批量缓存判定 ----
	step("6 批量缓存判定（去重护栏 AC-3.1）")
	textByID := map[string]string{}
	for _, s := range sents {
		textByID[s.ID] = s.Text
	}
	checkItems := make([]map[string]any, 0, len(sents))
	for _, s := range sents {
		checkItems = append(checkItems, map[string]any{
			"sentence_id": s.ID, "text": s.Text,
			"voice": "Nanami", "rate": 1.0, "style": "", "engine_ver": "azure-v1"})
	}
	checkBody, _ := json.Marshal(map[string]any{"items": checkItems})
	checkResp, err := post("/api/v1/audio/check", checkBody, true)
	if err != nil {
		return err
	}
	var miss []map[string]any
	for _, it := range checkResp["items"].([]any) {
		m := it.(map[string]any)
		if !m["exists"].(bool) {
			miss = append(miss, m)
		}
	}
	ttsChars = 0
	for _, m := range miss {
		ttsChars += len([]rune(textByID[m["sentence_id"].(string)]))
	}
	done(fmt.Sprintf("miss %d 句（命中 %d）", len(miss), len(sents)-len(miss)))

	// ---- 7. 客户端 TTS + 预签名上传 + 登记 ----
	step("7 直连 Azure TTS → 预签名上传 → 登记")
	okCount := 0
	for i, m := range miss {
		text := textByID[m["sentence_id"].(string)]
		audio, err := azure.Synthesize(context.Background(), text, "Nanami", 1.0)
		if err != nil {
			// AC-3.3：单句失败不阻塞其余
			fmt.Printf("   句 %d TTS 失败（跳过）: %v\n", i+1, err)
			continue
		}
		ck := m["cache_key"].(string)
		presBody, _ := json.Marshal(map[string]string{"cache_key": ck})
		presResp, err := post("/api/v1/audio/presign", presBody, true)
		if err != nil {
			return err
		}
		if err := httpPut(presResp["upload_url"].(string), audio); err != nil {
			fmt.Printf("   句 %d 上传失败（跳过）: %v\n", i+1, err)
			continue
		}
		regBody, _ := json.Marshal(map[string]any{
			"sentence_id": m["sentence_id"], "cache_key": ck,
			"voice": "Nanami", "rate": 1.0, "style": "", "engine_ver": "azure-v1",
			"duration_ms": 0, "size_bytes": len(audio)})
		if _, err := post("/api/v1/audio/register", regBody, true); err != nil {
			return err
		}
		okCount++
	}
	done("TTS 成功:", okCount, "/", len(miss), "句 ·", ttsChars, "字符")

	// ---- 8. 复拉全量，校验每句有音频 ----
	step("8 复拉全量校验音频挂载")
	full, err = get("/api/v1/lessons/" + lessonID)
	if err != nil {
		return err
	}
	withAudio, without := 0, 0
	var firstAudioID string
	for _, m := range full["materials"].([]any) {
		for _, s := range m.(map[string]any)["sentences"].([]any) {
			sm := s.(map[string]any)
			audios := sm["audios"].([]any)
			if len(audios) > 0 {
				withAudio++
				if firstAudioID == "" {
					firstAudioID = audios[0].(map[string]any)["id"].(string)
				}
			} else {
				without++
			}
		}
	}
	if withAudio == 0 {
		return fmt.Errorf("无任何句子挂载音频")
	}
	done("有音频:", withAudio, "句 / 无音频:", without, "句")

	// ---- 9. 音频下载（服务端流式代理，2026-10-27 改造）----
	step("9 下载首个音频（流式代理）")
	req, _ := http.NewRequest(http.MethodGet, base+"/api/v1/audio/"+firstAudioID+"/file", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	n, _ := io.Copy(io.Discard, resp.Body)
	ctHeader := resp.Header.Get("Content-Type")
	status := resp.StatusCode
	resp.Body.Close()
	if status != 200 || n == 0 {
		return fmt.Errorf("音频下载异常: HTTP %d, %d bytes", status, n)
	}
	if !strings.Contains(ctHeader, "audio") {
		return fmt.Errorf("Content-Type 异常: %s", ctHeader)
	}
	done(fmt.Sprintf("HTTP 200，回传 %d bytes (%s)", n, ctHeader))

	// ---- 10. 假录音上传（预签名 PUT，FR-9.7 同层存储）----
	step("10 录音预签名上传")
	recKey := "smoke-recording-test.webm"
	recURL, err := store.PresignedRecordingPUT(context.Background(), recKey, 5*time.Minute)
	if err != nil {
		return err
	}
	if err := httpPut(recURL, []byte("fake-recording-bytes")); err != nil {
		return err
	}
	done("recording bucket 上传 OK")

	// ---- 11. 复习闭环 ----
	step("11 复习入池 → 结果 → 到期列表")
	firstSent := sents[0].ID
	revBody, _ := json.Marshal(map[string]string{
		"sentence_id": firstSent, "source": "marked"})
	if _, err := post("/api/v1/reviews", revBody, true); err != nil {
		return err
	}
	dueResp, err := get("/api/v1/reviews/due?date=" + time.Now().AddDate(0, 0, 2).Format("2006-01-02"))
	if err != nil {
		return err
	}
	items := dueResp["items"].([]any)
	if len(items) == 0 {
		return fmt.Errorf("到期列表为空，入池失败")
	}
	revID := items[0].(map[string]any)["id"].(string)
	resBody, _ := json.Marshal(map[string]string{"result": "pass"})
	resResp, err := post("/api/v1/reviews/"+revID+"/result", resBody, true)
	if err != nil {
		return err
	}
	done("答对 → 新间隔:", resResp["interval_days"], "天 / 下次到期:", resResp["due_date"])

	fmt.Printf("\n========== 冒烟全部通过（总耗时 %.1fs）==========\n", time.Since(total).Seconds())
	fmt.Printf("LLM: in %d / out %d tokens · TTS: %d 字符 %d 句\n", llmIn, llmOut, ttsChars, okCount)
	fmt.Println("服务端闭环：auth → 词单 → LLM 材料 → schema 校验 + 振り仮名 → 落库 →")
	fmt.Println("           缓存判定 → Azure TTS → 预签名直传 → 登记 → 302 下载 → 录音层 → 复习调度")
	return nil
}

// wordKeyMap 构造 surface → isKey 映射（与服务端 lesson_word 一致的预校验输入）。
func wordKeyMap(words []map[string]any) map[string]bool {
	m := map[string]bool{}
	for _, w := range words {
		m[w["surface"].(string)], _ = w["is_key"].(bool)
	}
	return m
}

// ---- HTTP helpers ----

func post(path string, body []byte, auth bool) (map[string]any, error) {
	req, err := http.NewRequest(http.MethodPost, base+path, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	if auth {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, fmt.Errorf("%s 响应非 JSON: %.200s", path, raw)
	}
	if resp.StatusCode >= 300 {
		if b, ok := m["error"].(map[string]any); ok {
			return m, fmt.Errorf("%s HTTP %d: %v", path, resp.StatusCode, b["message"])
		}
		return m, fmt.Errorf("%s HTTP %d: %.200s", path, resp.StatusCode, raw)
	}
	return m, nil
}

func get(path string) (map[string]any, error) {
	req, err := http.NewRequest(http.MethodGet, base+path, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, fmt.Errorf("%s 响应非 JSON: %.200s", path, raw)
	}
	if resp.StatusCode >= 300 {
		return m, fmt.Errorf("%s HTTP %d", path, resp.StatusCode)
	}
	return m, nil
}

func httpPut(url string, data []byte) error {
	req, err := http.NewRequest(http.MethodPut, url, bytes.NewReader(data))
	if err != nil {
		return err
	}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, resp.Body)
	if resp.StatusCode >= 300 {
		return fmt.Errorf("PUT %s HTTP %d", truncate(url, 60), resp.StatusCode)
	}
	return nil
}

func mustEnv(key string) string {
	v, err := envcfg.MustGet(key)
	if err != nil {
		fmt.Fprintln(os.Stderr, "smoke:", err)
		os.Exit(1)
	}
	return v
}

func truncate(s string, n int) string {
	if len(s) > n {
		return s[:n]
	}
	return s
}

// smokeWords 内置 20 词 N5 词单（§7.1 场景向；2 个重点词）。
func smokeWords() []map[string]any {
	type w struct {
		surface, reading, pos, meaning string
		key                            bool
	}
	ws := []w{
		{"精算", "せいさん", "名詞", "一起结账", true},
		{"預ける", "あずける", "動詞", "寄存；存放", true},
		{"満員", "まんいん", "名詞", "满座、人满", false},
		{"駅", "えき", "名詞", "车站", false},
		{"切符", "きっぷ", "名詞", "票", false},
		{"掃除", "そうじ", "名詞", "清扫", false},
		{"洗濯", "せんたく", "名詞", "洗衣服", false},
		{"予約", "よやく", "名詞", "预约", false},
		{"用意", "ようい", "名詞", "准备", false},
		{"届く", "とどく", "動詞", "送达", false},
		{"選ぶ", "えらぶ", "動詞", "选择", false},
		{"疲れる", "つかれる", "動詞", "疲惫", false},
		{"寄る", "よる", "動詞", "顺路去", false},
		{"揃う", "そろう", "動詞", "齐全", false},
		{"貯金", "ちょきん", "名詞", "存钱", false},
		{"趣味", "しゅみ", "名詞", "爱好", false},
		{"頑張る", "がんばる", "動詞", "坚持、加油", false},
		{"家賃", "やちん", "名詞", "房租", false},
		{"説明", "せつめい", "名詞", "说明", false},
		{"替わる", "かわる", "動詞", "更换、替换", false},
	}
	out := make([]map[string]any, 0, len(ws))
	for _, x := range ws {
		out = append(out, map[string]any{
			"surface": x.surface, "reading": x.reading,
			"pos": x.pos, "meaning": x.meaning, "is_key": x.key})
	}
	return out
}

// buildPrompt 构造材料生成 prompt（FR-2 要求 → §5.3 schema）。
func buildPrompt(words []map[string]any) string {
	var b strings.Builder
	b.WriteString("你是日语听说学习材料的编写专家。请为下面的词汇表生成一份完整的 JSON 材料包。\n\n")
	b.WriteString("要求：\n")
	b.WriteString("- 水平 N5（语法与词汇尽量简单），全部句子用です・ます体，自然、贴近日常生活\n")
	b.WriteString("- 词汇表共 20 个词，每个词都必须自然地出现在至少一个句子里；")
	b.WriteString("某个词确实不适合场景时才允许在 word_occurrence 里标 \"skipped: 原因\"\n")
	b.WriteString("- 生成恰好 2 段材料：\n")
	b.WriteString("  1. id=m1，type=dialogue，场景「コンビニ」（便利店），8—10 句，speaker 用 A/B 交替，")
	b.WriteString("role_a=店員，role_b=客\n")
	b.WriteString("  2. id=m2，type=passage，主题「休日の家事」（休息日的家务），8—10 句独白，speaker=独白\n")
	b.WriteString("- 每个句子的 id 用 s1、s2、s3…（跨材料顺序编号）；sort_order 从 1 开始\n")
	b.WriteString("- 每句包含 text（日语）、kana（整句假名读音）、translation（中文翻译）\n")
	b.WriteString("- qa_items 恰好 5 个：围绕材料内容的口头问答（prompt、answer、prompt_translation）\n")
	b.WriteString("- substitution_items 恰好 5 个：围绕词表的造句任务（word_id、prompt_zh、answer）\n")
	b.WriteString("- word_occurrence：key 是词的 surface（如 \"精算\"），value 是该词出现的句子 id 数组（如 [\"s1\",\"s5\"]）或 \"skipped: 原因\"\n\n")
	b.WriteString("词汇表（surface｜读音｜中文｜标记）：\n")
	for _, w := range words {
		mark := "—"
		if key, _ := w["is_key"].(bool); key {
			mark = "★重点：出现 2 次"
		}
		b.WriteString(fmt.Sprintf("%s｜%s｜%s｜%s\n", w["surface"], w["reading"], w["meaning"], mark))
	}
	b.WriteString(`
严格按如下 JSON 结构输出（不要 markdown 围栏、不要任何解释文字）：
{
  "materials": [{"id":"m1","type":"dialogue","scene":"...","title":"...","role_a":"店員","role_b":"客","sort_order":1,
    "sentences":[{"id":"s1","sort_order":1,"speaker":"A","text":"...","kana":"...","translation":"...","para_no":0}]}],
  "qa_items": [{"prompt":"...","answer":"...","prompt_translation":"..."}],
  "substitution_items": [{"word_id":"精算","prompt_zh":"...","answer":"..."}],
  "word_occurrence": {"精算":["s1","s3"],"駅":["s2"]}
}`)
	return b.String()
}
