// 词典发音：按「词形 + 读音」合成并缓存到 OSS；单词用读音注音合成，
// 整句用 kagome 逐词读音合成，避免 Azure 对多音字自作主张。
package dict

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/greatgeek/TingShuo/backend/internal/auth"
	"github.com/greatgeek/TingShuo/backend/internal/generate"
	"github.com/greatgeek/TingShuo/backend/internal/httpx"
	"github.com/greatgeek/TingShuo/backend/pkg/storage"
	"github.com/greatgeek/TingShuo/backend/pkg/ttsclient"
)

var (
	audioLim   = httpx.NewLimiter(60, 20) // 每用户 60/分钟（仅合成未命中缓存时计）
	audioDaily = httpx.NewDailyQuota(1000)
)

// Audio GET /dict/audio?text=部屋&reading=へや
// text 必填；reading 可空（整句按词典分词读音合成）。
func (h *Handler) Audio(w http.ResponseWriter, r *http.Request) {
	userID := auth.FromContext(r.Context())
	text := strings.TrimSpace(r.URL.Query().Get("text"))
	reading := strings.TrimSpace(r.URL.Query().Get("reading"))
	if text == "" || !hasJapanese(text) || len([]rune(text)) > 120 {
		httpx.Err(w, httpx.BadRequest("BAD_TEXT", "发音文本不正确"))
		return
	}
	if reading != "" && (!isKana(reading) || len([]rune(reading)) > 120) {
		reading = ""
	}

	key := "dict-audio-v1-" + audioTag(text+"|"+reading)
	if data, ok := fetchOSS(r.Context(), h.store, key); ok {
		serveMP3(w, data)
		return
	}
	if !audioLim.Allow(userID) || !audioDaily.Allow(userID) {
		httpx.Err(w, httpx.NewError(http.StatusTooManyRequests, "QUOTA", "发音请求过于频繁，请稍后再试"))
		return
	}

	var audio []byte
	var err error
	if reading != "" {
		audio, err = h.azure.SynthesizeRuby(r.Context(), voiceName, rateValue,
			[]ttsclient.RubyToken{{Surface: text, Reading: reading}})
	} else {
		audio, err = h.azure.SynthesizeRuby(r.Context(), voiceName, rateValue, rubyTokensFor(text))
	}
	if err == nil && !ttsclient.ValidMP3(audio) {
		err = fmt.Errorf("invalid mp3 (%d bytes)", len(audio))
	}
	if err != nil { // Azure 偶发空体/坏流：重试一次
		if reading != "" {
			audio, err = h.azure.SynthesizeRuby(r.Context(), voiceName, rateValue,
				[]ttsclient.RubyToken{{Surface: text, Reading: reading}})
		} else {
			audio, err = h.azure.SynthesizeRuby(r.Context(), voiceName, rateValue, rubyTokensFor(text))
		}
	}
	if err != nil || !ttsclient.ValidMP3(audio) {
		httpx.Err(w, httpx.BadGateway("TTS 上游失败/坏音频: "+fmt.Sprint(err)))
		return
	}
	_ = h.store.PutAudio(r.Context(), key, audio)
	serveMP3(w, audio)
}

const (
	voiceName = "Nanami"
	rateValue = 1.0
)

// rubyTokensFor 用站内同一套分词读音合成整句；无读音词元用字面。
func rubyTokensFor(text string) []ttsclient.RubyToken {
	pairs := generate.BuildRubyTokens(text)
	out := make([]ttsclient.RubyToken, 0, len(pairs))
	for _, p := range pairs {
		if len(p) < 2 {
			continue
		}
		surface, _ := p[0].(string)
		if surface == "" {
			continue
		}
		reading, _ := p[1].(string)
		out = append(out, ttsclient.RubyToken{Surface: surface, Reading: reading})
	}
	if len(out) == 0 {
		return []ttsclient.RubyToken{{Surface: text}}
	}
	return out
}

func audioTag(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:12])
}

// fetchOSS / serveMP3 与词音管线同约定：对象不存在则回退合成，响应不回显 key。

func fetchOSS(ctx context.Context, store *storage.Client, key string) ([]byte, bool) {
	obj, err := store.GetObject(ctx, storage.AudioKey(key))
	if err != nil {
		return nil, false
	}
	defer obj.Close()
	data, err := io.ReadAll(obj)
	if err != nil || len(data) == 0 {
		return nil, false
	}
	return data, true
}

func serveMP3(w http.ResponseWriter, data []byte) {
	w.Header().Set("Content-Type", "audio/mpeg")
	w.Header().Set("Content-Length", fmt.Sprint(len(data)))
	w.Write(data)
}
