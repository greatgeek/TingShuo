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
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
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

const engineVer = "doubao-podcast-v13"

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

// HasJapanese 判断文本是否含假名（平/片/半角）。
// 注意：纯汉字日语词（如「食費」）无法与中文区分，v1 依赖生成端 prompt 将其归入日语行。
func HasJapanese(s string) bool {
	for _, r := range s {
		if (r >= 0x3040 && r <= 0x30FF) || (r >= 0xFF66 && r <= 0xFF9F) {
			return true
		}
	}
	return false
}

// Line 为脚本一行：role 决定音色，文本语言（中/日）由内容决定。
type Line struct {
	Role string `json:"role"`
	Text string `json:"text"`
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
		out = append(out, doubaopod.Line{Speaker: v, Text: l.Text})
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

// Generate POST /podcast/generate {lesson_id?, lines?}：生成一集 v13 播客并落库。
// lines 缺省时由 LLM 从 lesson_id 出稿；cache_key 命中直接返回既有集（cached=true）。
// 音频不经 JSON 返回：前端凭 episode_id 走 GET /podcast/{id}/file 流式播放。
func (h *Handler) Generate(w http.ResponseWriter, r *http.Request) {
	userID := auth.FromContext(r.Context())
	if !genLim.Allow(userID) || !genDaily.Allow(userID) {
		httpx.Err(w, httpx.NewError(http.StatusTooManyRequests, "QUOTA", "播客生成过于频繁（每日上限 20 集）"))
		return
	}
	var req struct {
		LessonID string `json:"lesson_id"`
		DirID    string `json:"dir_id"`
		Lines    []Line `json:"lines"`
	}
	if e := httpx.Decode(r, &req); e != nil {
		httpx.Err(w, e)
		return
	}
	lines := req.Lines
	llmModel := ""
	if len(lines) == 0 {
		if req.LessonID == "" && req.DirID == "" {
			httpx.Err(w, httpx.BadRequest("PARAMS_INVALID", "lesson_id / dir_id / lines 至少给一个"))
			return
		}
		var (
			in  Input
			err error
		)
		if req.DirID != "" {
			in, err = LoadDirInput(r.Context(), h.pool, userID, req.DirID)
		} else {
			in, err = LoadLessonInput(r.Context(), h.pool, userID, req.LessonID)
		}
		if err != nil {
			httpx.Err(w, httpx.NotFound(err.Error()))
			return
		}
		if len(in.Words) == 0 {
			httpx.Err(w, httpx.BadRequest("SOURCE_EMPTY", "该来源没有可用于播客的词"))
			return
		}
		lines, err = BuildScript(r.Context(), in)
		if err != nil {
			httpx.Err(w, httpx.NewError(http.StatusBadGateway, "SCRIPT_LLM", err.Error()))
			return
		}
		llmModel = os.Getenv("LLM_MODEL")
	} else if req.DirID != "" {
		if _, err := LoadDirInput(r.Context(), h.pool, userID, req.DirID); err != nil {
			httpx.Err(w, httpx.NotFound(err.Error()))
			return
		}
	} else if req.LessonID != "" {
		var n int
		if err := h.pool.QueryRow(r.Context(), `SELECT count(1) FROM daily_lesson WHERE id=$1 AND user_id=$2`, req.LessonID, userID).Scan(&n); err != nil || n == 0 {
			httpx.Err(w, httpx.NotFound("课程不存在或不属于当前用户"))
			return
		}
	}
	if len(lines) < 2 {
		httpx.Err(w, httpx.BadRequest("SCRIPT_TOO_SHORT", "脚本至少 2 行"))
		return
	}
	sessionLines, err := SessionLines(lines)
	if err != nil {
		httpx.Err(w, httpx.BadRequest("SCRIPT_INVALID", err.Error()))
		return
	}

	ck := cacheKey(req.LessonID, req.DirID, lines)
	var (
		epID       string
		durationMS int
		rounds     int
		tlJSON     []byte
		srt        string
	)
	if err := h.pool.QueryRow(r.Context(),
		`SELECT id, duration_ms, rounds, timeline, srt FROM podcast_episode WHERE cache_key=$1`, ck).
		Scan(&epID, &durationMS, &rounds, &tlJSON, &srt); err == nil {
		var tl []Segment
		json.Unmarshal(tlJSON, &tl)
		httpx.WriteJSON(w, http.StatusOK, map[string]any{
			"episode_id": epID, "cached": true, "timeline": tl, "srt": srt,
			"duration_ms": durationMS, "rounds": rounds,
		})
		return
	}

	t0 := time.Now()
	roundBufs, err := h.pod.Session(r.Context(), [2]string{doubaopod.VoiceSisi, doubaopod.VoiceXudong}, sessionLines, nil)
	if err != nil {
		httpx.Err(w, httpx.NewError(http.StatusBadGateway, "PODCAST_UPSTREAM", err.Error()))
		return
	}
	audio, tl, err := AssembleRounds(roundBufs, lines)
	if err != nil {
		httpx.Err(w, httpx.BadRequest("ASSEMBLE_INVALID", err.Error()))
		return
	}
	epID = ulid.MustNew()
	if err := h.store.PutAudio(r.Context(), "podcast-"+epID, audio); err != nil {
		httpx.Err(w, httpx.BadGateway("对象存储写入失败: "+err.Error()))
		return
	}
	tlJSON, _ = json.Marshal(tl)
	srt = SRT(tl)
	chars := 0
	for _, l := range lines {
		chars += len([]rune(l.Text))
	}
	_, err = h.pool.Exec(r.Context(), `
		INSERT INTO podcast_episode (id, user_id, lesson_id, dir_id, cache_key, script, timeline, srt, storage_key, duration_ms, rounds, llm_model)
		VALUES ($1,$2,NULLIF($3,''),NULLIF($4,''),$5,$6,$7,$8,$9,$10,$11,NULLIF($12,''))`,
		epID, userID, req.LessonID, req.DirID, ck, mustJSON(lines), tlJSON, srt, storage.AudioKey("podcast-"+epID),
		tl[len(tl)-1].EndMS, len(roundBufs), llmModel)
	if err != nil {
		httpx.Err(w, httpx.BadGateway("落库失败: "+err.Error()))
		return
	}
	h.pool.Exec(r.Context(), `
		INSERT INTO tts_call_log (id, user_id, voice, chars, duration_ms, cache_hit)
		VALUES ($1,$2,'doubao-podcast-v13',$3,$4,false)`,
		ulid.MustNew(), userID, chars, time.Since(t0).Milliseconds())

	httpx.WriteJSON(w, http.StatusOK, map[string]any{
		"episode_id": epID, "cached": false, "timeline": tl, "srt": srt,
		"duration_ms": tl[len(tl)-1].EndMS, "rounds": len(roundBufs),
	})
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
	lines, err := BuildScript(r.Context(), in)
	if err != nil {
		httpx.Err(w, httpx.NewError(http.StatusBadGateway, "SCRIPT_LLM", err.Error()))
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"lines": lines, "input": in})
}
