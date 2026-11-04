// Package podcast 实现 v13 单场双语播客：思思（女主持）+ xudong（男主持），两人均中日双语。
// 2026-11-04 用户终选 v13（样片 v1—v13 耳朵判定）。
//
// 设计依据：
//   - 播客每场限 2 音色；saturn 播客系列（咪仔/大壹）读日文带中文音 → 弃用；
//   - multi/moon 会话音色（思思、xudong）中日双语原生，同场无拼接、无音色切换；
//   - 语言分配是**内容层**决策（哪行用日语说），由生成端 prompt 控制，服务端不做语言硬校验；
//   - SSE 事件 360/362 按轮切分，轮序=脚本行序 → 时间轴零对齐成本。
package podcast

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/greatgeek/TingShuo/backend/internal/auth"
	"github.com/greatgeek/TingShuo/backend/internal/httpx"
	"github.com/greatgeek/TingShuo/backend/pkg/doubaopod"
	"github.com/greatgeek/TingShuo/backend/pkg/storage"
	"github.com/greatgeek/TingShuo/backend/pkg/ulid"
	"github.com/jackc/pgx/v5/pgxpool"
)

const engineVer = "doubao-podcast-v13-kana"

var (
	genLim   = httpx.NewLimiter(5, 10) // 每用户 5/分钟（播客会话成本高）
	genDaily = httpx.NewDailyQuota(20) // 每用户 20/天
)

// 角色：a=思思（女主持），b=xudong（男主持）。两人均可说中文或日语。
const (
	RoleA = "a"
	RoleB = "b"
)

type Handler struct {
	pool  *pgxpool.Pool
	pod   *doubaopod.Client
	store *storage.Client
}

func NewHandler(pool *pgxpool.Pool, pod *doubaopod.Client, store *storage.Client) *Handler {
	return &Handler{pool: pool, pod: pod, store: store}
}

// Line 为脚本一行：role 决定音色；Text 为展示文本（字幕/列表，可含日语汉字），
// Say 为合成文本（缺省时服务端用词表把汉字形替换成假名读音，保证日语发音，2026-11-04 用户方案）。
type Line struct {
	Role string `json:"role"`
	Text string `json:"text"`
	Say  string `json:"say,omitempty"`
}

// Segment 为时间轴一项（逐轮高亮/字幕的数据源）。
type Segment struct {
	Role    string `json:"role"`
	Text    string `json:"text"`
	StartMS int64  `json:"start_ms"`
	EndMS   int64  `json:"end_ms"`
}

// voiceOf 角色→音色。
func voiceOf(role string) (string, error) {
	switch role {
	case RoleA:
		return doubaopod.VoiceSisi, nil
	case RoleB:
		return doubaopod.VoiceXudong, nil
	default:
		return "", fmt.Errorf("role 必须为 a/b，收到 %q", role)
	}
}

// Kanaize 将文本中出现的词表汉字形替换为假名读音（合成文本用）；展示文本保持汉字。
// 例：中文行「食費」就是伙食费 → 合成文本「しょくひ」就是伙食费，中文主持也按日语读音念该词。
// 按词形长度降序替换，避免短词先命中破坏长词（外食 被 食 吃掉）；单字词跳过，
// 否则会误伤中文行文里的同形字（伙食费 里的 食）。
func Kanaize(text string, words []WordBrief) string {
	out := text
	for _, w := range sortedBySurfaceLen(words) {
		if w.Reading == "" || w.Surface == w.Reading || len([]rune(w.Surface)) < 2 {
			continue
		}
		out = strings.ReplaceAll(out, w.Surface, w.Reading)
	}
	return out
}

func sortedBySurfaceLen(words []WordBrief) []WordBrief {
	out := append([]WordBrief(nil), words...)
	sort.SliceStable(out, func(i, j int) bool {
		return len([]rune(out[i].Surface)) > len([]rune(out[j].Surface))
	})
	return out
}

// SessionLines 校验脚本并映射为播客会话行。
func SessionLines(lines []Line) ([]doubaopod.Line, error) {
	out := make([]doubaopod.Line, 0, len(lines))
	for i, l := range lines {
		if strings.TrimSpace(l.Text) == "" {
			return nil, fmt.Errorf("第 %d 行文本为空", i)
		}
		v, err := voiceOf(l.Role)
		if err != nil {
			return nil, fmt.Errorf("第 %d 行: %w", i, err)
		}
		txt := l.Say
		if strings.TrimSpace(txt) == "" {
			txt = l.Text
		}
		out = append(out, doubaopod.Line{Speaker: v, Text: txt})
	}
	return out, nil
}

// AssembleRounds 将按轮切分的音频还原为整集并计算时间轴（mp3 逐帧时长，精确到轮）。
func AssembleRounds(rounds [][]byte, lines []Line) ([]byte, []Segment, error) {
	if len(rounds) != len(lines) {
		return nil, nil, fmt.Errorf("轮数 %d 与脚本行数 %d 不一致", len(rounds), len(lines))
	}
	var (
		audio    []byte
		tl       []Segment
		cursorMS int64
	)
	for i, r := range rounds {
		d := doubaopod.MP3DurationMS(r)
		audio = append(audio, r...)
		tl = append(tl, Segment{Role: lines[i].Role, Text: lines[i].Text, StartMS: cursorMS, EndMS: cursorMS + d})
		cursorMS += d
	}
	return audio, tl, nil
}

// SRT 由时间轴导出字幕（参照 ai-podcast-workflow 的 SRT/VTT 产物）。
func SRT(tl []Segment) string {
	var sb strings.Builder
	for i, s := range tl {
		sb.WriteString(fmt.Sprintf("%d\n%s --> %s\n%s\n\n", i+1, ts(s.StartMS), ts(s.EndMS), s.Text))
	}
	return sb.String()
}

func ts(ms int64) string {
	return fmt.Sprintf("%02d:%02d:%02d,%03d", ms/3600000, ms%3600000/60000, ms%60000/1000, ms%1000)
}

// cacheKey 为「源(课/目录)+脚本+引擎版本」的稳定哈希：同源同脚本零重复计费（AC-3.1 同款护栏）。
func cacheKey(lessonID, dirID string, lines []Line) string {
	h := sha256.New()
	io.WriteString(h, engineVer+"\x00L:"+lessonID+"\x00D:"+dirID+"\x00")
	for _, l := range lines {
		io.WriteString(h, l.Role+"\x01"+l.Text+"\x00")
	}
	return hex.EncodeToString(h.Sum(nil))
}

type genReq struct {
	LessonID string `json:"lesson_id"`
	DirID    string `json:"dir_id"`
	Lines    []Line `json:"lines"`
}

// Meta 为一集的返回元数据（JSON 与 SSE done 事件共用）。
type Meta struct {
	EpisodeID  string    `json:"episode_id"`
	Cached     bool      `json:"cached"`
	Timeline   []Segment `json:"timeline"`
	SRT        string    `json:"srt"`
	DurationMS int64     `json:"duration_ms"`
	Rounds     int       `json:"rounds"`
}

// generateCore 生成或取缓存一集；emit 非 nil 时推送进度事件（事件名, data）。
func (h *Handler) generateCore(ctx context.Context, userID string, req genReq, emit func(string, map[string]any)) (*Meta, *httpx.APIError) {
	stage := func(msg string) {
		if emit != nil {
			emit("stage", map[string]any{"msg": msg})
		}
	}
	lines := req.Lines
	llmModel := ""
	var srcWords []WordBrief
	if len(lines) == 0 {
		if req.LessonID == "" && req.DirID == "" {
			return nil, httpx.BadRequest("PARAMS_INVALID", "lesson_id / dir_id / lines 至少给一个")
		}
		stage("读取原料…")
		var (
			in  Input
			err error
		)
		if req.DirID != "" {
			in, err = LoadDirInput(ctx, h.pool, userID, req.DirID)
		} else {
			in, err = LoadLessonInput(ctx, h.pool, userID, req.LessonID)
		}
		if err != nil {
			return nil, httpx.NotFound(err.Error())
		}
		if len(in.Words) == 0 {
			return nil, httpx.BadRequest("SOURCE_EMPTY", "该来源没有可用于播客的词")
		}
		srcWords = in.Words
		stage(fmt.Sprintf("LLM 写稿中（%d 词 %d 句入料）…", len(in.Words), len(in.Sentences)))
		lines, err = BuildScript(ctx, in, func(think, content int) {
			if emit == nil {
				return
			}
			if content == 0 {
				emit("script", map[string]any{"chars": content, "msg": fmt.Sprintf("构思中… 已思考 %d 字", think)})
			} else {
				emit("script", map[string]any{"chars": content, "msg": fmt.Sprintf("写稿中… 已生成 %d 字", content)})
			}
		})
		if err != nil {
			return nil, httpx.NewError(http.StatusBadGateway, "SCRIPT_LLM", err.Error())
		}
		llmModel = os.Getenv("LLM_MODEL")
		stage(fmt.Sprintf("写稿完成：%d 行对话", len(lines)))
	} else if req.DirID != "" {
		if _, err := LoadDirInput(ctx, h.pool, userID, req.DirID); err != nil {
			return nil, httpx.NotFound(err.Error())
		}
	} else if req.LessonID != "" {
		var n int
		if err := h.pool.QueryRow(ctx, `SELECT count(1) FROM daily_lesson WHERE id=$1 AND user_id=$2`, req.LessonID, userID).Scan(&n); err != nil || n == 0 {
			return nil, httpx.NotFound("课程不存在或不属于当前用户")
		}
	}
	if len(lines) < 2 {
		return nil, httpx.BadRequest("SCRIPT_TOO_SHORT", "脚本至少 2 行")
	}
	for i := range lines {
		if lines[i].Say == "" {
			lines[i].Say = Kanaize(lines[i].Text, srcWords)
		}
	}
	sessionLines, err := SessionLines(lines)
	if err != nil {
		return nil, httpx.BadRequest("SCRIPT_INVALID", err.Error())
	}

	ck := cacheKey(req.LessonID, req.DirID, lines)
	var (
		epID       string
		durationMS int
		rounds     int
		tlJSON     []byte
		srt        string
	)
	if err := h.pool.QueryRow(ctx,
		`SELECT id, duration_ms, rounds, timeline, srt FROM podcast_episode WHERE cache_key=$1`, ck).
		Scan(&epID, &durationMS, &rounds, &tlJSON, &srt); err == nil {
		var tl []Segment
		json.Unmarshal(tlJSON, &tl)
		return &Meta{EpisodeID: epID, Cached: true, Timeline: tl, SRT: srt, DurationMS: int64(durationMS), Rounds: rounds}, nil
	}

	t0 := time.Now()
	n := len(sessionLines)
	stage(fmt.Sprintf("合成中 0/%d 轮…", n))
	roundBufs, err := h.pod.Session(ctx, [2]string{doubaopod.VoiceSisi, doubaopod.VoiceXudong}, sessionLines, nil, func(i int) {
		if emit != nil {
			emit("round", map[string]any{"i": i + 1, "n": n, "msg": fmt.Sprintf("合成中 %d/%d 轮…", i+1, n)})
		}
	})
	if err != nil {
		return nil, httpx.NewError(http.StatusBadGateway, "PODCAST_UPSTREAM", err.Error())
	}
	stage("装配音频与时间轴…")
	audio, tl, err := AssembleRounds(roundBufs, lines)
	if err != nil {
		return nil, httpx.BadRequest("ASSEMBLE_INVALID", err.Error())
	}
	stage("落库与缓存…")
	epID = ulid.MustNew()
	if err := h.store.PutAudio(ctx, "podcast-"+epID, audio); err != nil {
		return nil, httpx.BadGateway("对象存储写入失败: " + err.Error())
	}
	tlJSON, _ = json.Marshal(tl)
	srt = SRT(tl)
	chars := 0
	for _, l := range lines {
		chars += len([]rune(l.Text))
	}
	_, err = h.pool.Exec(ctx, `
		INSERT INTO podcast_episode (id, user_id, lesson_id, dir_id, cache_key, script, timeline, srt, storage_key, duration_ms, rounds, llm_model, tts_engine)
		VALUES ($1,$2,NULLIF($3,''),NULLIF($4,''),$5,$6,$7,$8,$9,$10,$11,NULLIF($12,''),$13)`,
		epID, userID, req.LessonID, req.DirID, ck, mustJSON(lines), tlJSON, srt, storage.AudioKey("podcast-"+epID),
		tl[len(tl)-1].EndMS, len(roundBufs), llmModel, engineVer)
	if err != nil {
		return nil, httpx.BadGateway("落库失败: " + err.Error())
	}
	h.pool.Exec(ctx, `
		INSERT INTO tts_call_log (id, user_id, voice, chars, duration_ms, cache_hit)
		VALUES ($1,$2,$3,$4,$5,false)`,
		ulid.MustNew(), userID, engineVer, chars, time.Since(t0).Milliseconds())
	return &Meta{EpisodeID: epID, Cached: false, Timeline: tl, SRT: srt, DurationMS: tl[len(tl)-1].EndMS, Rounds: len(roundBufs)}, nil
}

// Generate POST /podcast/generate：一次性 JSON 返回（脚本/工具用）。
func (h *Handler) Generate(w http.ResponseWriter, r *http.Request) {
	userID := auth.FromContext(r.Context())
	if !genLim.Allow(userID) || !genDaily.Allow(userID) {
		httpx.Err(w, httpx.NewError(http.StatusTooManyRequests, "QUOTA", "播客生成过于频繁（每日上限 20 集）"))
		return
	}
	var req genReq
	if e := httpx.Decode(r, &req); e != nil {
		httpx.Err(w, e)
		return
	}
	meta, aerr := h.generateCore(r.Context(), userID, req, nil)
	if aerr != nil {
		httpx.Err(w, aerr)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, meta)
}

// Stream POST /podcast/generate/stream：SSE 进度流（前端滚动日志用）。
// 事件：stage{msg} / round{i,n,msg} / done{Meta} / error{message} / end{}
func (h *Handler) Stream(w http.ResponseWriter, r *http.Request) {
	userID := auth.FromContext(r.Context())
	if !genLim.Allow(userID) || !genDaily.Allow(userID) {
		httpx.Err(w, httpx.NewError(http.StatusTooManyRequests, "QUOTA", "播客生成过于频繁（每日上限 20 集）"))
		return
	}
	var req genReq
	if e := httpx.Decode(r, &req); e != nil {
		httpx.Err(w, e)
		return
	}
	fl, ok := w.(http.Flusher)
	if !ok {
		httpx.Err(w, httpx.NewError(http.StatusInternalServerError, "NO_STREAM", "不支持流式响应"))
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Accel-Buffering", "no") // nginx 不缓冲，进度实时到端
	emit := func(ev string, data map[string]any) {
		b, _ := json.Marshal(data)
		fmt.Fprintf(w, "event: %s\ndata: %s\n\n", ev, b)
		fl.Flush()
	}
	meta, aerr := h.generateCore(r.Context(), userID, req, emit)
	if aerr != nil {
		emit("error", map[string]any{"message": aerr.Message})
	} else {
		emit("done", map[string]any{
			"episode_id": meta.EpisodeID, "cached": meta.Cached, "timeline": meta.Timeline,
			"srt": meta.SRT, "duration_ms": meta.DurationMS, "rounds": meta.Rounds,
		})
	}
	emit("end", map[string]any{})
}

// Latest GET /podcast/by-lesson/{lesson_id}：该课最新一集元数据（无则 404）。
func (h *Handler) Latest(w http.ResponseWriter, r *http.Request) {
	userID := auth.FromContext(r.Context())
	lessonID := chi.URLParam(r, "lesson_id")
	var (
		epID       string
		durationMS int
		rounds     int
		tlJSON     []byte
		srt        string
	)
	err := h.pool.QueryRow(r.Context(), `
		SELECT e.id, e.duration_ms, e.rounds, e.timeline, e.srt
		FROM podcast_episode e JOIN daily_lesson l ON l.id = e.lesson_id
		WHERE e.lesson_id = $1 AND l.user_id = $2
		ORDER BY e.created_at DESC LIMIT 1`, lessonID, userID).
		Scan(&epID, &durationMS, &rounds, &tlJSON, &srt)
	if err != nil {
		httpx.Err(w, httpx.NotFound("该课还没有播客"))
		return
	}
	var tl []Segment
	json.Unmarshal(tlJSON, &tl)
	httpx.WriteJSON(w, http.StatusOK, map[string]any{
		"episode_id": epID, "timeline": tl, "srt": srt,
		"duration_ms": durationMS, "rounds": rounds,
	})
}

// LatestDir GET /podcast/by-dir/{dir_id}：该生词本目录最新一集元数据（无则 404）。
func (h *Handler) LatestDir(w http.ResponseWriter, r *http.Request) {
	userID := auth.FromContext(r.Context())
	dirID := chi.URLParam(r, "dir_id")
	var (
		epID       string
		durationMS int
		rounds     int
		tlJSON     []byte
		srt        string
	)
	err := h.pool.QueryRow(r.Context(), `
		SELECT e.id, e.duration_ms, e.rounds, e.timeline, e.srt
		FROM podcast_episode e
		JOIN notebook_dir d ON d.id = e.dir_id
		JOIN notebook nb ON nb.id = d.notebook_id
		WHERE e.dir_id = $1 AND nb.user_id = $2
		ORDER BY e.created_at DESC LIMIT 1`, dirID, userID).
		Scan(&epID, &durationMS, &rounds, &tlJSON, &srt)
	if err != nil {
		httpx.Err(w, httpx.NotFound("该单元还没有播客"))
		return
	}
	var tl []Segment
	json.Unmarshal(tlJSON, &tl)
	httpx.WriteJSON(w, http.StatusOK, map[string]any{
		"episode_id": epID, "timeline": tl, "srt": srt,
		"duration_ms": durationMS, "rounds": rounds,
	})
}

// File GET /podcast/{id}/file：流式代理该集音频（归属校验同 tts.File）。
func (h *Handler) File(w http.ResponseWriter, r *http.Request) {
	userID := auth.FromContext(r.Context())
	epID := chi.URLParam(r, "id")
	var objectKey string
	if err := h.pool.QueryRow(r.Context(),
		`SELECT storage_key FROM podcast_episode WHERE id=$1 AND user_id=$2`, epID, userID).
		Scan(&objectKey); err != nil {
		httpx.Err(w, httpx.NotFound("播客不存在或不属于当前用户"))
		return
	}
	obj, err := h.store.GetObject(r.Context(), objectKey)
	if err != nil {
		httpx.Err(w, httpx.BadGateway("对象存储读取失败: "+err.Error()))
		return
	}
	defer obj.Close()
	w.Header().Set("Content-Type", "audio/mpeg")
	w.Header().Set("Cache-Control", "private, max-age=86400")
	io.Copy(w, obj)
}

func mustJSON(v any) []byte {
	b, _ := json.Marshal(v)
	return b
}

// Script POST /podcast/script {lesson_id}：由一课的词/句生成 v13 行级脚本（不合成音频）。
// 与 /podcast/generate 分离：脚本可先眼检/人工改，再送合成。
func (h *Handler) Script(w http.ResponseWriter, r *http.Request) {
	userID := auth.FromContext(r.Context())
	var req struct {
		LessonID string `json:"lesson_id"`
	}
	if e := httpx.Decode(r, &req); e != nil {
		httpx.Err(w, e)
		return
	}
	if req.LessonID == "" {
		httpx.Err(w, httpx.BadRequest("PARAMS_INVALID", "lesson_id 不能为空"))
		return
	}
	in, err := LoadLessonInput(r.Context(), h.pool, userID, req.LessonID)
	if err != nil {
		httpx.Err(w, httpx.NotFound(err.Error()))
		return
	}
	if len(in.Words) == 0 {
		httpx.Err(w, httpx.BadRequest("LESSON_EMPTY", "该课没有可用于播客的词"))
		return
	}
	lines, err := BuildScript(r.Context(), in, nil)
	if err != nil {
		httpx.Err(w, httpx.NewError(http.StatusBadGateway, "SCRIPT_LLM", err.Error()))
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"lines": lines, "input": in})
}
