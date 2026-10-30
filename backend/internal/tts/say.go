// Say POST /audio/say {text, voice?, rate?}：任意文本合成 MP3（生词本例句等不在 sentence 表的文本）。
// 与 /audio/synthesize 的区别：不登记 audio_asset、不绑句子；按 (text,voice,rate) 哈希落 text_audio 缓存，
// 同文本同参数零重复计费。
package tts

import (
	"fmt"
	"io"
	"net/http"
	"strconv"

	"github.com/greatgeek/TingShuo/backend/internal/generate"
	"github.com/greatgeek/TingShuo/backend/internal/httpx"
	"github.com/greatgeek/TingShuo/backend/pkg/storage"
	"github.com/greatgeek/TingShuo/backend/pkg/ttsclient"
)

func (h *Handler) Say(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Text  string  `json:"text"`
		Voice string  `json:"voice"`
		Rate  float64 `json:"rate"`
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
	if req.Rate == 0 {
		req.Rate = 1.0
	}
	if len([]rune(req.Text)) > 200 {
		httpx.Err(w, httpx.BadRequest("TEXT_TOO_LONG", "文本过长（>200 字）"))
		return
	}
	if generate.HasSimplifiedChinese(req.Text) {
		httpx.Err(w, httpx.BadRequest("TEXT_NOT_JA", "文本含中文，拒绝合成"))
		return
	}

	ck := CacheKey(req.Text, Params{Voice: req.Voice, Rate: req.Rate, Style: "", EngineVer: "say-v1"})
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
		audio, serr = h.azure.Synthesize(r.Context(), req.Text, req.Voice, req.Rate)
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

