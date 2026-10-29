// 修复生词本句子文本的两类解析残留（在 import 之前跑）：
//  1) 复合词被交错注音拆散：「日に本」「佐さ藤」「二に年」→ 用 kagome 判定
//     （单汉字+1-2假名+单汉字 且两汉字合成一个词、其读音以该假名开头）→ 删掉中间假名碎片。
//  2) 孤立全角括号：只有 ） 没有 （（或反之）时整行清除该种括号。
// 用法: node 管线之后、import 之前：go run ./cmd/textrepair <fullJson>
package main

import (
	"encoding/json"
	"fmt"
	"os"
	"regexp"
	"strings"

	"github.com/greatgeek/TingShuo/backend/internal/generate"
)

var fragRe = regexp.MustCompile(`([一-鿯])([ぁ-ん]{1,2})([一-鿯])`)

func repair(text string) (string, bool) {
	orig := text
	if !strings.Contains(text, "（") {
		text = strings.ReplaceAll(text, "）", "")
	}
	if !strings.Contains(text, "）") {
		text = strings.ReplaceAll(text, "（", "")
	}
	// 复合词假名碎片：从左往右扫；未合并时只前进到 mid 之后，保留 k3 供重叠候选匹配
	var b strings.Builder
	rest := text
	for {
		m := fragRe.FindStringSubmatchIndex(rest)
		if m == nil {
			b.WriteString(rest)
			break
		}
		k1 := rest[m[2]:m[3]]
		mid := rest[m[4]:m[5]]
		k3 := rest[m[6]:m[7]]
		b.WriteString(rest[:m[2]])
		merged := false
		// 规则 A：mid 恰为 k1 单字读音（佐さ藤 / 二に年）→ mid 是 k1 的注音碎片
		if t1 := generate.BuildRubyTokens(k1); len(t1) == 1 {
			if rd, ok := t1[0][1].(string); ok && rd == mid {
				merged = true
			}
		}
		// 规则 B：k1+k3 是一个词且其读音以 mid 开头（日に本 / 科か学）
		if !merged {
			if t2 := generate.BuildRubyTokens(k1 + k3); len(t2) == 1 {
				if rd, ok := t2[0][1].(string); ok && rd != "" && strings.HasPrefix(rd, mid) && rd != mid {
					merged = true
				}
			}
		}
		if merged {
			b.WriteString(k1 + k3)
			rest = rest[m[7]:]
		} else {
			b.WriteString(k1 + mid)
			rest = rest[m[5]:]
		}
	}
	return b.String(), b.String() != orig
}

func main() {
	path := os.Args[1]
	raw, err := os.ReadFile(path)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	// 用 map 结构读改写（保留全部字段）
	var full map[string]any
	if err := json.Unmarshal(raw, &full); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	n := 0
	lessons, _ := full["lessons"].([]any)
	for _, li := range lessons {
		L, _ := li.(map[string]any)
		cats, _ := L["categories"].([]any)
		for _, ci := range cats {
			C, _ := ci.(map[string]any)
			words, _ := C["words"].([]any)
			for _, wi := range words {
				W, _ := wi.(map[string]any)
				sents, _ := W["sentences"].([]any)
				for _, si := range sents {
					S, _ := si.(map[string]any)
					t, _ := S["text"].(string)
					if nt, changed := repair(t); changed {
						S["text"] = nt
						n++
					}
				}
			}
		}
	}
	out, _ := json.MarshalIndent(full, "", " ")
	if err := os.WriteFile(path, out, 0644); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	fmt.Printf("修复句子文本 %d 处\n", n)
}
