package wordclip

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/greatgeek/TingShuo/backend/pkg/llmclient"
)

type clipText struct {
	ZH      string
	SenseOK bool
}

type translateItem struct {
	I   int    `json:"i"`
	Ja  string `json:"ja"`
	Hit string `json:"hit"`
}

// translateClips 一次 LLM 调用翻译整词的所有候选句，并判断句中词义是否与词单一致。
// 返回按候选下标索引的结果；LLM 未配置/失败时返回 error，由调用方回退英文。
func translateClips(ctx context.Context, surface, reading, meaning string, cands []candidate) (map[int]clipText, error) {
	// 翻译只处理少量短句，不需要 LLM 客户端的 10 分钟长材料超时；
	// 单个词卡住会堵住整条队列，超时直接回退英文
	ctx, cancel := context.WithTimeout(ctx, 90*time.Second)
	defer cancel()
	cli := llmclient.New(
		strings.TrimSuffix(os.Getenv("LLM_BASE_URL"), "/"),
		os.Getenv("LLM_API_KEY"),
		os.Getenv("LLM_MODEL"),
	)
	if cli == nil {
		return nil, fmt.Errorf("LLM 未配置")
	}
	items := make([]translateItem, 0, len(cands))
	for i, c := range cands {
		items = append(items, translateItem{I: i, Ja: c.seg.TextJa.Content, Hit: c.matchText})
	}
	rawItems, _ := json.Marshal(items)
	meaningLine := "词义未知，只做翻译，sense_ok 一律 true"
	if strings.TrimSpace(meaning) != "" {
		meaningLine = "词义：" + strings.TrimSpace(meaning)
	}
	prompt := fmt.Sprintf(`你是日语教材编辑。目标词「%s」（读音：%s，%s）。
下面是包含该词的日剧/动画台词。对每条：
1) 翻成自然、准确的中文；只输出中文，不要出现日文假名（人名地名保留汉字）；
2) 判断台词中的这个词是否就是上面的词义。同形异义、只是复合词/惯用表达的一部分导致意思不同时，sense_ok 必须为 false。
只输出 JSON：{"items":[{"i":0,"zh":"中文翻译","sense_ok":true}]}，不要解释，不要遗漏条目。
台词：%s`, surface, reading, meaningLine, string(rawItems))

	content, _, _, err := cli.ChatJSON(ctx, prompt)
	if err != nil {
		return nil, err
	}
	var out struct {
		Items []struct {
			I       int    `json:"i"`
			ZH      string `json:"zh"`
			SenseOK bool   `json:"sense_ok"`
		} `json:"items"`
	}
	if err := json.Unmarshal([]byte(llmclient.ExtractJSON(content)), &out); err != nil {
		return nil, err
	}
	res := map[int]clipText{}
	for _, it := range out.Items {
		if it.I < 0 || it.I >= len(cands) {
			continue
		}
		zh := strings.TrimSpace(it.ZH)
		if zh == "" || len([]rune(zh)) > 120 || containsKana(zh) {
			continue
		}
		res[it.I] = clipText{ZH: zh, SenseOK: strings.TrimSpace(meaning) == "" || it.SenseOK}
	}
	if len(res) == 0 {
		return nil, fmt.Errorf("LLM 未返回可用翻译")
	}
	return res, nil
}

func containsKana(s string) bool {
	for _, r := range s {
		if (r >= 0x3041 && r <= 0x3096) || (r >= 0x30A1 && r <= 0x30FA) {
			return true
		}
	}
	return false
}
