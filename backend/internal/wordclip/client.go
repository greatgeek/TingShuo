// Package wordclip 为生词补充影视原声例句片段。
//
// 数据源为 Nadeshiko（api.nadeshiko.co）：按词搜索日剧/动画台词，取回
// 视频/音频/截图直链与毫秒起止。搜索每词一次（take 20），结果落 word_clip
// 缓存后所有用户复用；播放走 CDN 直链，不消耗配额。
//
// 生命周期：词入库 → word_clip_queue 入队（去重）→ 后台 worker 逐词搜索 →
// LLM 翻中文并校验词义 → 落 word_clip → 前端按需取 2 条轮换展示。
package wordclip

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"
)

const defaultBase = "https://api.nadeshiko.co"

type Client struct {
	base string
	key  string
	http *http.Client
}

// NewClient 读取 NADE_API_KEY；未配置返回 nil，worker 与接口随之静默停用。
func NewClient() *Client {
	key := strings.TrimSpace(os.Getenv("NADE_API_KEY"))
	if key == "" {
		return nil
	}
	base := strings.TrimSuffix(strings.TrimSpace(os.Getenv("NADE_API_BASE")), "/")
	if base == "" {
		base = defaultBase
	}
	return &Client{
		base: base,
		key:  key,
		http: &http.Client{Timeout: 25 * time.Second},
	}
}

// APIError 区分 429 的两种语义：每分钟限流可退避重试，月度配额耗尽必须停。
type APIError struct {
	Status  int
	Code    string
	Message string
}

func (e *APIError) Error() string {
	return fmt.Sprintf("nadeshiko HTTP %d %s: %s", e.Status, e.Code, e.Message)
}

func (e *APIError) QuotaExceeded() bool { return e.Status == 429 && e.Code == "QUOTA_EXCEEDED" }
func (e *APIError) RateLimited() bool   { return e.Status == 429 && e.Code != "QUOTA_EXCEEDED" }

type Token struct {
	S        string `json:"s"`
	D        string `json:"d"`
	R        string `json:"r"`
	B        int    `json:"b"`
	E        int    `json:"e"`
	Kind     string `json:"kind"`
	Pt       string `json:"pt"`
	PosLabel string `json:"posLabel"`
}

type Segment struct {
	PublicID      string `json:"publicId"`
	Status        string `json:"status"`
	ContentRating string `json:"contentRating"`
	StartTimeMs   int    `json:"startTimeMs"`
	EndTimeMs     int    `json:"endTimeMs"`
	Episode       int    `json:"episode"`
	EpisodeTitle  string `json:"episodeTitle"`
	MediaPublicID string `json:"mediaPublicId"`
	TextJa        struct {
		Content   string  `json:"content"`
		Highlight string  `json:"highlight"`
		Tokens    []Token `json:"tokens"`
	} `json:"textJa"`
	TextEn struct {
		Content string `json:"content"`
	} `json:"textEn"`
	Urls struct {
		ImageURL string `json:"imageUrl"`
		AudioURL string `json:"audioUrl"`
		VideoURL string `json:"videoUrl"`
	} `json:"urls"`
}

type mediaInfo struct {
	NameJa     string `json:"nameJa"`
	NameRomaji string `json:"nameRomaji"`
	NameEn     string `json:"nameEn"`
	Category   string `json:"category"`
}

func (m mediaInfo) displayName() string {
	for _, s := range []string{m.NameJa, m.NameEn, m.NameRomaji} {
		if strings.TrimSpace(s) != "" {
			return strings.TrimSpace(s)
		}
	}
	return ""
}

// mediaSet 兼容 includes.media 的两种形态：以 publicId 为键的对象，或数组。
type mediaSet map[string]mediaInfo

func (s *mediaSet) UnmarshalJSON(data []byte) error {
	*s = mediaSet{}
	if len(data) == 0 || string(data) == "null" {
		return nil
	}
	var byID map[string]mediaInfo
	if err := json.Unmarshal(data, &byID); err == nil {
		*s = byID
		return nil
	}
	// 数组形态：逐项从 publicId 建索引
	var rawList []map[string]json.RawMessage
	if err := json.Unmarshal(data, &rawList); err != nil {
		return err
	}
	for _, raw := range rawList {
		var id string
		_ = json.Unmarshal(raw["publicId"], &id)
		var info mediaInfo
		buf, _ := json.Marshal(raw)
		_ = json.Unmarshal(buf, &info)
		if id != "" {
			(*s)[id] = info
		}
	}
	return nil
}

type searchResponse struct {
	Segments []Segment `json:"segments"`
	Includes struct {
		Media mediaSet `json:"media"`
	} `json:"includes"`
}

type Quota struct {
	Used      int `json:"used"`
	Limit     int `json:"limit"`
	Remaining int `json:"remaining"`
	Period    int `json:"periodYyyymm"`
}

// Search 按词搜索短句。每调用一次消耗 1 次月度配额。
func (c *Client) Search(ctx context.Context, query string, take int) ([]Segment, mediaSet, error) {
	if take <= 0 || take > 50 {
		take = 20
	}
	body := map[string]any{
		"query": map[string]any{"search": query, "exactMatch": false},
		"take":  take,
		"sort":  map[string]any{"mode": "RELEVANCE"},
		"filters": map[string]any{
			"category":           []string{"ANIME", "JDRAMA"},
			"contentRating":      []string{"SAFE"},
			"status":             []string{"ACTIVE"},
			"segmentLengthChars": map[string]int{"min": 4, "max": 50},
			"segmentDurationMs":  map[string]int{"min": 1500, "max": 10000},
		},
		"include": []string{"media"},
	}
	var out searchResponse
	if err := c.do(ctx, http.MethodPost, "/v1/search", body, &out); err != nil {
		return nil, nil, err
	}
	return out.Segments, out.Includes.Media, nil
}

// Me 查询当月配额（不消耗搜索配额）。
func (c *Client) Me(ctx context.Context) (Quota, error) {
	var out struct {
		Quota Quota `json:"quota"`
	}
	err := c.do(ctx, http.MethodGet, "/v1/user/me", nil, &out)
	return out.Quota, err
}

func (c *Client) do(ctx context.Context, method, path string, body any, out any) error {
	var rdr io.Reader
	if body != nil {
		buf, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rdr = bytes.NewReader(buf)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, rdr)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.key)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 500))
		var e struct {
			Code   string `json:"code"`
			Title  string `json:"title"`
			Detail string `json:"detail"`
		}
		_ = json.Unmarshal(raw, &e)
		msg := e.Detail
		if msg == "" {
			msg = e.Title
		}
		if msg == "" {
			msg = strings.TrimSpace(string(raw))
		}
		return &APIError{Status: resp.StatusCode, Code: e.Code, Message: msg}
	}
	return json.NewDecoder(io.LimitReader(resp.Body, 4<<20)).Decode(out)
}
