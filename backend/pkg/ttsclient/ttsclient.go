// Package ttsclient 提供 Azure Speech TTS REST 客户端（BYOK：Key 由调用方从本地 .env 提供）。
// 端点使用专用域名 {region}.tts.speech.microsoft.com（2024+ 新资源不挂统一端点，见开发问题记录 P-013）。
package ttsclient

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

const outputFormat = "audio-24khz-48kbitrate-mono-mp3" // FR-3.8：MP3 24kHz 48kbps

type Client struct {
	apiKey string
	region string
	http   *http.Client
}

func New(apiKey, region string) *Client {
	return &Client{
		apiKey: apiKey,
		region: region,
		http:   &http.Client{Timeout: 30 * time.Second},
	}
}

// Synthesize 按句生成 MP3（§8.4 SSML 模板；P0 不带 express-as 风格）。
func (c *Client) Synthesize(ctx context.Context, text, voice string, rate float64) ([]byte, error) {
	if voice == "" {
		voice = "ja-JP-NanamiNeural" // FR-3.1 默认音色
	}
	ratePct := fmt.Sprintf("%g%%", (rate-1)*100) // 0.85 → -15%，1.15 → +15%
	ssml := fmt.Sprintf(`<speak version="1.0" xml:lang="ja-JP" xmlns="http://www.w3.org/2001/10/synthesis">`+
		`<voice name="ja-JP-%sNeural"><prosody rate="%s">%s</prosody></voice></speak>`,
		voice, ratePct, escapeXML(text))

	endpoint := fmt.Sprintf("https://%s.tts.speech.microsoft.com/cognitiveservices/v1", c.region)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(ssml))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Ocp-Apim-Subscription-Key", c.apiKey)
	req.Header.Set("Content-Type", "application/ssml+xml")
	req.Header.Set("X-Microsoft-OutputFormat", outputFormat)
	req.Header.Set("User-Agent", "TingShuo-smoke")

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("TTS 请求失败: %w", err)
	}
	defer resp.Body.Close()
	audio, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("TTS 返回 %d: %s", resp.StatusCode, truncate(string(audio), 200))
	}
	return audio, nil
}

func escapeXML(s string) string {
	r := strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;", `"`, "&#34;", "'", "&#39;")
	return r.Replace(s)
}

func truncate(s string, n int) string {
	if len(s) > n {
		return s[:n]
	}
	return s
}
