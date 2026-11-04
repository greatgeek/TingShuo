// Package doubaopod 提供火山豆包「语音播客」SSE 客户端（资源 10050，action=3 脚本直读）。
// 2026-11-03 样片验证结论（v1—v11）：
//   - 播客每场限 2 音色；saturn 播客系列（咪仔/大壹）为中文音色，读日文带中文音；
//   - 每行语言控制的唯一通道是 speaker 分配（日语行分给 ja 原生音色）；
//   - SSE 事件 360=轮开始 / 362=轮结束，可按轮切分音频 → v10 混合装配的数据基础。
//
// 本包只负责「一场会话 → 每轮音频缓冲」，装配策略在 internal/podcast。
package doubaopod

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"
)

const endpoint = "https://openspeech.bytedance.com/api/v3/podcast/sse"

// 已验证音色（2026-11-03）：saturn=播客专属中文音色；uranus ja=日语原生（TTS 服务亦可用）。
const (
	VoiceSisi   = "multi_female_shuangkuaisisi_moon_bigtts"    // 思思：女主持（中日双语，v13 定稿）
	VoiceXudong = "multi_male_xudong_conversation_wvae_bigtts" // xudong：男主持（中日双语，v13 定稿）
	VoiceMizai  = "zh_female_mizaitongxue_v2_saturn_bigtts"    // 咪仔：中文主持（v12 弃用：读日文带中文音）
	VoiceDayi   = "zh_male_dayixiansheng_v2_saturn_bigtts"     // 大壹：中文捧哏（v10/v12 弃用）
	VoiceMinimi = "ja_female_minimi_uranus_bigtts"             // 日语原生示范（v10 弃用）
	VoiceBv524  = "ja_male_bv524_uranus_bigtts"                // 日语原生男（v10 弃用占位）
)

// AdditionsUranus：uranus 系列音色挂播客服务时需显式指定 additions 模型（样片验证）。
const AdditionsUranus = "seed-tts-2.0-standard"

// 火山文档硬限：单轮 ≤300 字符、单场所有轮次合计 ≤10000 字符（max_char_length_per_round）。
// 预算取 4000：一场约 13 分钟音频、3 分钟合成，失败重试的代价小；
// 40 分钟长集因此自动拆成 3 场（2026-11-04 实测 309 字/分钟、240 轮单场通过）。
const (
	maxCharsPerRound   = 300
	maxCharsPerSession = 4000
)

type Client struct {
	apiKey string
	http   *http.Client
}

func New(apiKey string) *Client {
	return &Client{apiKey: apiKey, http: &http.Client{Timeout: 900 * time.Second}}
}

// Line 为脚本一行：Speaker 决定该行音色（亦即该行语言）。
type Line struct {
	Speaker string `json:"speaker"`
	Text    string `json:"text"`
}

type sessionReq struct {
	Action       int    `json:"action"`
	NlpTexts     []Line `json:"nlp_texts"`
	ReturnURL    bool   `json:"return_audio_url"`
	OnlyNlpText  bool   `json:"only_nlp_text"`
	UseHeadMusic bool   `json:"use_head_music"`
	AudioConfig  struct {
		Format     string `json:"format"`
		SampleRate int    `json:"sample_rate"`
	} `json:"audio_config"`
	SpeakerInfo struct {
		RandomOrder bool              `json:"random_order"`
		Speakers    []string          `json:"speakers"`
		Additions   map[string]string `json:"speaker_additions,omitempty"`
	} `json:"speaker_info"`
}

// Session 跑完整脚本并按字符预算自动拆场（单场硬限见 maxCharsPerSession），
// 返回按轮（360→362）切分的 MP3 缓冲：rounds[i] 对应 lines[i]（action=3 保持脚本顺序，
// 样片验证 8/8、3/3、240/240 对齐）。onRound 非 nil 时每完成一轮回调**全局**轮下标。
// 同一对 speaker 跨场音色一致（同一 speaker ID 即固定音色）；use_head_music 全程关闭，
// 否则续场会在中间插开场音效。
func (c *Client) Session(ctx context.Context, speakers [2]string, lines []Line, additions map[string]string, onRound func(int)) ([][]byte, error) {
	if c.apiKey == "" {
		return nil, fmt.Errorf("DOUBAO_TTS_API_KEY 未配置")
	}
	if len(lines) == 0 {
		return nil, fmt.Errorf("脚本为空")
	}
	var (
		all   [][]byte
		batch []Line
		chars int
	)
	run := func() error {
		if len(batch) == 0 {
			return nil
		}
		base := len(all)
		var rounds [][]byte
		var err error
		for attempt := 0; attempt < 2; attempt++ {
			rounds, err = c.session(ctx, speakers, batch, additions, func(i int) {
				if onRound != nil {
					onRound(base + i)
				}
			})
			if err == nil {
				break
			}
		}
		if err != nil {
			return fmt.Errorf("第 %d-%d 轮合成失败: %w", base+1, base+len(batch), err)
		}
		all = append(all, rounds...)
		batch, chars = nil, 0
		return nil
	}
	for i, l := range lines {
		n := len([]rune(l.Text))
		if n > maxCharsPerRound {
			return nil, fmt.Errorf("第 %d 行 %d 字超过单轮上限 %d 字", i, n, maxCharsPerRound)
		}
		if chars+n > maxCharsPerSession {
			if err := run(); err != nil {
				return nil, err
			}
		}
		batch = append(batch, l)
		chars += n
	}
	if err := run(); err != nil {
		return nil, err
	}
	return all, nil
}

// session 跑一场会话（一场 ≤ maxCharsPerSession 字符）。
func (c *Client) session(ctx context.Context, speakers [2]string, lines []Line, additions map[string]string, onRound func(int)) ([][]byte, error) {
	var req sessionReq
	req.Action = 3
	req.NlpTexts = lines
	req.AudioConfig.Format = "mp3"
	req.AudioConfig.SampleRate = 24000
	req.SpeakerInfo.Speakers = speakers[:]
	req.SpeakerInfo.Additions = additions

	body, err := json.Marshal(req)
	if err != nil {
		return nil, err
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Accept", "text/event-stream")
	httpReq.Header.Set("x-api-key", c.apiKey)
	httpReq.Header.Set("x-api-resource-id", "volc.service_type.10050")
	httpReq.Header.Set("x-api-request-id", fmt.Sprintf("%d", time.Now().UnixNano()))

	resp, err := c.http.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("播客请求失败: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("播客返回 %d", resp.StatusCode)
	}

	var (
		bufs []*bytes.Buffer
		cur  *bytes.Buffer
	)
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 1024*1024), 8*1024*1024)
	for sc.Scan() {
		line := sc.Text()
		if strings.HasPrefix(line, "event:") {
			switch strings.TrimSpace(strings.TrimPrefix(line, "event:")) {
			case "360":
				cur = &bytes.Buffer{}
				bufs = append(bufs, cur)
			case "362":
				if onRound != nil && len(bufs) > 0 {
					onRound(len(bufs) - 1)
				}
				cur = nil
			}
			continue
		}
		if !strings.HasPrefix(line, "data:") || cur == nil {
			continue
		}
		var ev struct {
			Code int    `json:"code"`
			Msg  string `json:"message"`
			Data string `json:"data"`
		}
		if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data:")), &ev); err != nil {
			continue
		}
		if ev.Code != 0 && ev.Code != 20000000 {
			return nil, fmt.Errorf("播客事件错误 code=%d %s", ev.Code, ev.Msg)
		}
		if ev.Data == "" {
			continue
		}
		raw, err := base64.StdEncoding.DecodeString(ev.Data)
		if err != nil {
			return nil, fmt.Errorf("音频 base64 解码失败: %w", err)
		}
		cur.Write(raw)
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}

	rounds := make([][]byte, 0, len(bufs))
	for i, b := range bufs {
		if b.Len() == 0 {
			return nil, fmt.Errorf("第 %d 轮音频为空（轮次切分失败）", i)
		}
		rounds = append(rounds, bytes.Clone(b.Bytes()))
	}
	if len(rounds) != len(lines) {
		return nil, fmt.Errorf("轮次数 %d 与脚本行数 %d 不一致", len(rounds), len(lines))
	}
	return rounds, nil
}
