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

// cacheKey 为「源(课/目录) + 原料词表 + 引擎版本」的稳定哈希：同源同词表零重复计费。
// 不再把脚本行纳入哈希——长片写稿要几分钟且每次措辞都不同，按脚本哈希等于永不去重。
func cacheKey(lessonID, dirID string, words []WordBrief) string {
	h := sha256.New()
	io.WriteString(h, engineVer+"\x00L:"+lessonID+"\x00D:"+dirID+"\x00")
	for _, w := range words {
		io.WriteString(h, w.Surface+"\x01"+w.Reading+"\x01"+w.Meaning+"\x00")
		for _, s := range w.Sentences {
			io.WriteString(h, s.Text+"\x00")
		}
	}
	return hex.EncodeToString(h.Sum(nil))
}

// Meta 为一集的返回元数据。
type Meta struct {
	EpisodeID  string    `json:"episode_id"`
	Cached     bool      `json:"cached"`
	Timeline   []Segment `json:"timeline"`
	SRT        string    `json:"srt"`
	DurationMS int64     `json:"duration_ms"`
	Rounds     int       `json:"rounds"`
}

// Prog 为生成进度（写回 podcast_job，前端轮询显示）。
type Prog struct {
	Status        string
	Stage         string
	Seg, Segs     int
	Round, Rounds int
}

// loadInput 按源（目录优先）取原料，含归属校验。
func (h *Handler) loadInput(ctx context.Context, userID, lessonID, dirID string) (Input, error) {
	if dirID != "" {
		return LoadDirInput(ctx, h.pool, userID, dirID)
	}
	if lessonID != "" {
		return LoadLessonInput(ctx, h.pool, userID, lessonID)
	}
	return Input{}, fmt.Errorf("lesson_id / dir_id 至少给一个")
}

// generate 跑完一集：读原料 → 查缓存 → 分段写稿 → 汉字转假名 → 拆场合成 → 装配落库。
// 由后台 worker 调用（不在 HTTP 请求生命周期内），onProg 可为 nil。
func (h *Handler) generate(ctx context.Context, userID, lessonID, dirID string, onProg func(Prog)) (*Meta, error) {
	prog := func(p Prog) {
		if onProg != nil {
			onProg(p)
		}
	}
	in, err := h.loadInput(ctx, userID, lessonID, dirID)
	if err != nil {
		return nil, err
	}
	if len(in.Words) == 0 {
		return nil, fmt.Errorf("该来源没有可用于播客的词")
	}

	ck := cacheKey(lessonID, dirID, in.Words)
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
		prog(Prog{Status: JobDone, Stage: "已有成片，直接复用"})
		return &Meta{EpisodeID: epID, Cached: true, Timeline: tl, SRT: srt, DurationMS: int64(durationMS), Rounds: rounds}, nil
	}

	segs := len(planSegments(in.Words))
	prog(Prog{Status: JobWriting, Stage: fmt.Sprintf("写稿中（%d 词 %d 段）…", len(in.Words), segs), Segs: segs})
	lines, err := BuildScript(ctx, in, func(p Progress) {
		prog(Prog{
			Status: JobWriting, Seg: p.Seg, Segs: p.Total,
			Stage: fmt.Sprintf("写稿中：已完成 %d/%d 段（刚写完 %s）", p.Seg, p.Total, p.Label),
		})
	})
	if err != nil {
		return nil, err
	}
	for i := range lines {
		if lines[i].Say == "" {
			lines[i].Say = Kanaize(lines[i].Text, in.Words)
		}
	}
	sessionLines, err := SessionLines(lines)
	if err != nil {
		return nil, err
	}

	t0 := time.Now()
	n := len(sessionLines)
	prog(Prog{Status: JobSynth, Stage: fmt.Sprintf("写稿完成 %d 行，合成中 0/%d 轮…", len(lines), n),
		Seg: segs, Segs: segs, Rounds: n})
	roundBufs, err := h.pod.Session(ctx, [2]string{doubaopod.VoiceSisi, doubaopod.VoiceXudong}, sessionLines, nil, func(i int) {
		prog(Prog{Status: JobSynth, Stage: fmt.Sprintf("合成中 %d/%d 轮…", i+1, n),
			Seg: segs, Segs: segs, Round: i + 1, Rounds: n})
	})
	if err != nil {
		return nil, err
	}
	prog(Prog{Status: JobSynth, Stage: "装配音频与时间轴…", Seg: segs, Segs: segs, Round: n, Rounds: n})
	audio, tl, err := AssembleRounds(roundBufs, lines)
	if err != nil {
		return nil, err
	}
	epID = ulid.MustNew()
	if err := h.store.PutAudio(ctx, "podcast-"+epID, audio); err != nil {
		return nil, fmt.Errorf("对象存储写入失败: %w", err)
	}
	tlJSON, _ = json.Marshal(tl)
	srt = SRT(tl)
	chars := 0
	for _, l := range lines {
		chars += len([]rune(l.Text))
	}
	if _, err := h.pool.Exec(ctx, `
		INSERT INTO podcast_episode (id, user_id, lesson_id, dir_id, cache_key, script, timeline, srt, storage_key, duration_ms, rounds, llm_model, tts_engine)
		VALUES ($1,$2,NULLIF($3,''),NULLIF($4,''),$5,$6,$7,$8,$9,$10,$11,NULLIF($12,''),$13)`,
		epID, userID, lessonID, dirID, ck, mustJSON(lines), tlJSON, srt, storage.AudioKey("podcast-"+epID),
		tl[len(tl)-1].EndMS, len(roundBufs), os.Getenv("LLM_MODEL"), engineVer); err != nil {
		return nil, fmt.Errorf("落库失败: %w", err)
	}
	h.pool.Exec(ctx, `
		INSERT INTO tts_call_log (id, user_id, voice, chars, duration_ms, cache_hit)
		VALUES ($1,$2,$3,$4,$5,false)`,
		ulid.MustNew(), userID, engineVer, chars, time.Since(t0).Milliseconds())
	return &Meta{EpisodeID: epID, Cached: false, Timeline: tl, SRT: srt, DurationMS: tl[len(tl)-1].EndMS, Rounds: len(roundBufs)}, nil
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
