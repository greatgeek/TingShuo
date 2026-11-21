// Say POST /audio/say {text, voice?, rate?, ruby?}：任意文本合成 MP3（生词本例句等不在 sentence 表的文本）。
// ruby 非空时按注音纠读多音字（SSML <sub>）；与 /audio/synthesize 的区别：不登记 audio_asset、不绑句子；
// 按 (text,voice,rate,读音) 哈希落 text_audio 缓存，同文本同参数零重复计费。
package tts

import (
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"

	"github.com/greatgeek/TingShuo/backend/internal/auth"
	"github.com/greatgeek/TingShuo/backend/internal/generate"
	"github.com/greatgeek/TingShuo/backend/internal/httpx"
	"github.com/greatgeek/TingShuo/backend/pkg/storage"
	"github.com/greatgeek/TingShuo/backend/pkg/ttsclient"
)

var (
	sayLim   = httpx.NewLimiter(30, 60) // 每用户 30/分钟
	sayDaily = httpx.NewDailyQuota(500) // 每用户 500/天（TTS 成本护栏）
)

func (h *Handler) Say(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Text  string      `json:"text"`
		Voice string      `json:"voice"`
		Rate  float64     `json:"rate"`
		Ruby  [][]*string `json:"ruby"` // [[表记, 读音|null], ...]：按注音纠多音字
	}
	if e := httpx.Decode(r, &req); e != nil {
		httpx.Err(w, e)
		return
	}
	if req.Text == "" {
		httpx.Err(w, httpx.BadRequest("TEXT_EMPTY", "text 不能为空"))
		return
	}
	if req.Voice == "" {
		req.Voice = "Nanami"
	}
	// 音色白名单：日语 Nanami + 中文晓晓（连播"播中文含义"用）；其余一律拒，防乱传音色刷费用
	if req.Voice != "Nanami" && req.Voice != "zh-CN-XiaoxiaoNeural" {
		httpx.Err(w, httpx.BadRequest("VOICE_INVALID", "不支持的音色"))
		return
	}
	if req.Rate == 0 {
		req.Rate = 1.0
	}
	if len([]rune(req.Text)) > 200 {
		httpx.Err(w, httpx.BadRequest("TEXT_TOO_LONG", "文本过长（>200 字）"))
		return
	}
	// 中文音色就是来合成中文的；日语音色仍拒中文（避免串味坏音频落缓存）
	if req.Voice == "Nanami" && generate.HasSimplifiedChinese(req.Text) {
		httpx.Err(w, httpx.BadRequest("TEXT_NOT_JA", "文本含中文，拒绝合成"))
		return
	}

	userID := auth.FromContext(r.Context())
	if !sayLim.Allow(userID) || !sayDaily.Allow(userID) {
		httpx.Err(w, httpx.NewError(http.StatusTooManyRequests, "QUOTA", "合成请求过于频繁，请稍后再试（每日上限 500 次）"))
		return
	}

	// 注音有效且确有纠读时：合成走 <sub>，缓存键带上读音，避免命中旧的误读音频
	tokens, spoken, rubyOK := parseRuby(req.Text, req.Ruby)
	useRuby := rubyOK && spoken != req.Text
	ckText := req.Text
	if useRuby {
		// rb2：alias 改片假名后旧缓存音频仍是「は＋くぶつかん」的坏读法，必须换键重合成
		ckText += "\x1frb2:" + spoken
	}

	ck := CacheKey(ckText, Params{Voice: req.Voice, Rate: req.Rate, Style: "", EngineVer: "say-v1"})
	key := "say-" + ck // OSS 对象 key 前缀，与句子音频/单词音频区分

	// OSS 缓存命中；DB 只留元信息（空对象视为未命中，自愈重合成）
	if obj, err := h.store.GetObject(r.Context(), storage.AudioKey(key)); err == nil {
		data, rerr := io.ReadAll(obj)
		obj.Close()
		if rerr == nil && len(data) > 0 {
			w.Header().Set("Content-Type", "audio/mpeg")
			w.Header().Set("Content-Length", strconv.Itoa(len(data)))
			w.Write(data)
			return
		}
	}

	// Azure 偶发空体/连接失效/坏流：重试一次；坏音频绝不落缓存
	var audio []byte
	var serr error
	for i := 0; i < 2; i++ {
		if useRuby {
			audio, serr = h.azure.SynthesizeRuby(r.Context(), req.Voice, req.Rate, tokens)
		} else {
			audio, serr = h.azure.Synthesize(r.Context(), req.Text, req.Voice, req.Rate)
		}
		if serr == nil && ttsclient.ValidMP3(audio) {
			break
		}
		if serr == nil {
			serr = fmt.Errorf("invalid mp3 (%d bytes)", len(audio))
		}
	}
	if serr != nil {
		httpx.Err(w, httpx.BadGateway("TTS 生成失败: "+serr.Error()))
		return
	}
	_ = h.store.PutAudio(r.Context(), key, audio)
	_, _ = h.pool.Exec(r.Context(), `
		INSERT INTO text_audio (cache_key, voice, rate) VALUES ($1, $2, $3)
		ON CONFLICT (cache_key) DO UPDATE SET voice = EXCLUDED.voice, rate = EXCLUDED.rate`,
		ck, req.Voice, req.Rate)

	w.Header().Set("Content-Type", "audio/mpeg")
	w.Header().Set("Content-Length", strconv.Itoa(len(audio)))
	w.Write(audio)
}

// parseRuby 校验前端注音：各 token 表记拼起来必须与 text 完全一致，否则整组丢弃退回纯文本，
// 防止 SSML 念出的内容与页面展示不一致。返回值 spoken 为按注音展开后的读音串。
func parseRuby(text string, in [][]*string) ([]ttsclient.RubyToken, string, bool) {
	if len(in) == 0 {
		return nil, "", false
	}
	var surfaces, spoken strings.Builder
	tokens := make([]ttsclient.RubyToken, 0, len(in))
	for _, p := range in {
		if len(p) == 0 || p[0] == nil {
			return nil, "", false
		}
		s := *p[0]
		r := ""
		if len(p) > 1 && p[1] != nil {
			r = *p[1]
		}
		tokens = append(tokens, ttsclient.RubyToken{Surface: s, Reading: r})
		surfaces.WriteString(s)
		if r != "" && r != s {
			spoken.WriteString(r)
		} else {
			spoken.WriteString(s)
		}
	}
	if surfaces.String() != text {
		return nil, "", false
	}
	return tokens, spoken.String(), true
}
