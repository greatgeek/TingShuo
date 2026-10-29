// Say POST /audio/say {text, voice?, rate?}：任意文本合成 MP3（生词本例句等不在 sentence 表的文本）。
// 与 /audio/synthesize 的区别：不登记 audio_asset、不绑句子；按 (text,voice,rate) 哈希落 text_audio 缓存，
// 同文本同参数零重复计费。
package tts

import (
	"net/http"
	"strconv"

	"github.com/greatgeek/TingShuo/backend/internal/httpx"
	"github.com/jackc/pgx/v5"
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

	ck := CacheKey(req.Text, Params{Voice: req.Voice, Rate: req.Rate, Style: "", EngineVer: "say-v1"})

	var cached []byte
	err := h.pool.QueryRow(r.Context(),
		`SELECT data FROM text_audio WHERE cache_key = $1`, ck).Scan(&cached)
	if err == nil {
		w.Header().Set("Content-Type", "audio/mpeg")
		w.Header().Set("Content-Length", strconv.Itoa(len(cached)))
		w.Write(cached)
		return
	}
	if err != pgx.ErrNoRows {
		httpx.Err(w, httpx.Internal(err.Error()))
		return
	}

	audio, serr := h.azure.Synthesize(r.Context(), req.Text, req.Voice, req.Rate)
	if serr != nil {
		httpx.Err(w, httpx.BadGateway("TTS 生成失败: "+serr.Error()))
		return
	}
	_, _ = h.pool.Exec(r.Context(), `
		INSERT INTO text_audio (cache_key, voice, rate, data) VALUES ($1, $2, $3, $4)
		ON CONFLICT (cache_key) DO NOTHING`, ck, req.Voice, req.Rate, audio)

	w.Header().Set("Content-Type", "audio/mpeg")
	w.Header().Set("Content-Length", strconv.Itoa(len(audio)))
	w.Write(audio)
}

