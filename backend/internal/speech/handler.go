// Package speech 提供口头产出用的短语音识别代理。
// 复用 Azure Speech 的 key/region；浏览器录音在本地转成 16kHz 单声道 16bit WAV 后上传，
// 服务端只转发识别、不保存音频。识别结果仅供关键词命中提示，不参与评分。
package speech

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/greatgeek/TingShuo/backend/internal/auth"
	"github.com/greatgeek/TingShuo/backend/internal/httpx"
)

const maxAudioBytes = 2 << 20 // 15 秒 16kHz 单声道 WAV 约 480KB，留足余量

type Handler struct {
	key    string
	region string
	http   *http.Client
	lim    *httpx.Limiter
	daily  *httpx.DailyQuota
}

func NewHandler(key, region string) *Handler {
	return &Handler{
		key:    key,
		region: region,
		http:   &http.Client{Timeout: 20 * time.Second},
		lim:    httpx.NewLimiter(8, 8),  // 8 次/分钟
		daily:  httpx.NewDailyQuota(60), // 60 次/天，防费用失控
	}
}

// Recognize POST /speech/recognize —— body 为 WAV 原始字节。
// 返回 {text,status}；没听清时 text 为空、status=NoMatch。
func (h *Handler) Recognize(w http.ResponseWriter, r *http.Request) {
	userID := auth.FromContext(r.Context())
	if h.key == "" || h.region == "" {
		httpx.Err(w, httpx.NewError(http.StatusServiceUnavailable, "SPEECH_UNAVAILABLE", "语音识别未配置"))
		return
	}

	r.Body = http.MaxBytesReader(w, r.Body, maxAudioBytes)
	data, err := io.ReadAll(r.Body)
	if err != nil {
		var tooBig *http.MaxBytesError
		if errors.As(err, &tooBig) {
			httpx.Err(w, httpx.NewError(http.StatusRequestEntityTooLarge, "AUDIO_TOO_BIG", "录音过长，请控制在 15 秒内"))
			return
		}
		httpx.Err(w, httpx.BadRequest("AUDIO_READ_FAILED", "录音读取失败"))
		return
	}
	if !validWAV16k(data) {
		httpx.Err(w, httpx.BadRequest("AUDIO_FORMAT", "录音需为 16kHz 单声道 16 位 WAV"))
		return
	}

	if !h.lim.Allow(userID) || !h.daily.Allow(userID) {
		httpx.Err(w, httpx.NewError(http.StatusTooManyRequests, "QUOTA", "口头练习太频繁，请稍后再试（每日上限 60 次）"))
		return
	}

	text, status, err := h.recognize(r.Context(), data)
	if err != nil {
		httpx.Err(w, httpx.BadGateway("语音识别失败，请稍后再试"))
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"text": text, "status": status})
}

// validWAV16k 校验标准 44 字节 WAV 头：PCM / 单声道 / 16000Hz / 16bit。
func validWAV16k(b []byte) bool {
	if len(b) < 44 || string(b[0:4]) != "RIFF" || string(b[8:12]) != "WAVE" {
		return false
	}
	return binary.LittleEndian.Uint16(b[20:22]) == 1 &&
		binary.LittleEndian.Uint16(b[22:24]) == 1 &&
		binary.LittleEndian.Uint32(b[24:28]) == 16000 &&
		binary.LittleEndian.Uint16(b[34:36]) == 16
}

func (h *Handler) recognize(ctx context.Context, data []byte) (string, string, error) {
	u := fmt.Sprintf("https://%s.stt.speech.microsoft.com/speech/recognition/conversation/cognitiveservices/v1?language=ja-JP", h.region)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u, bytes.NewReader(data))
	if err != nil {
		return "", "", err
	}
	req.Header.Set("Ocp-Apim-Subscription-Key", h.key)
	req.Header.Set("Content-Type", "audio/wav; codecs=audio/pcm; samplerate=16000")
	req.Header.Set("Accept", "application/json")
	resp, err := h.http.Do(req)
	if err != nil {
		return "", "", err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return "", "", err
	}
	if resp.StatusCode != http.StatusOK {
		return "", "", fmt.Errorf("azure stt http %d", resp.StatusCode)
	}
	var out struct {
		RecognitionStatus string `json:"RecognitionStatus"`
		DisplayText       string `json:"DisplayText"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		return "", "", err
	}
	return strings.TrimSpace(out.DisplayText), out.RecognitionStatus, nil
}
