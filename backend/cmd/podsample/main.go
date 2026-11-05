// podsample 本地跑一集 v13 播客（思思+xudong 双双语）并落盘 mp3/时间轴/SRT，供耳检与装配回归。
// 用法：go run ./cmd/podsample [out.mp3]（默认 G:/TingShuo/podcast-samples/go-v13.mp3）
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"time"

	"github.com/greatgeek/TingShuo/backend/internal/podcast"
	"github.com/greatgeek/TingShuo/backend/pkg/doubaopod"
	"github.com/greatgeek/TingShuo/backend/pkg/envcfg"
)

// fixedScript 为人工定稿脚本（v13 样片同款）；POD_SCRIPT_LLM=1 时改用 LLM 出稿。
var fixedScript = []podcast.Line{
	{Role: podcast.RoleA, Text: "嗯……今天这个词，你月底一定想得起。"},
	{Role: podcast.RoleB, Text: "哈哈，是它吧。我先用日语说一遍——"},
	{Role: podcast.RoleB, Text: "食費。しょくひ、です。"},
	{Role: podcast.RoleA, Text: "しょくひ。"},
	{Role: podcast.RoleA, Text: "意思是吃饭花的钱，伙食费。"},
	{Role: podcast.RoleB, Text: "场景来了：月底，便利店门口。听——"},
	{Role: podcast.RoleB, Text: "今月、外食が多い。なので、食費が増えました。"},
	{Role: podcast.RoleA, Text: "今月、外食が多い。"},
	{Role: podcast.RoleA, Text: "这个月外食太多，所以伙食费涨了。一句话，两个短句。"},
	{Role: podcast.RoleB, Text: "记住它，下次聊钱的时候就用得上。"},
	{Role: podcast.RoleA, Text: "下期见！"},
}

// mixVariants 为"中日混读怎么落地"的对比带（2026-11-05 耳朵选型用）：
// 同一段教学内容，三种语言排布。已证实双语音色碰到假名会把整句切去日语，
// 句内混读不成立，所以候选只有"按行分语言"和"句拆成同人连续两行"。
// Say 预置 = 禁止 FillSay 假名化（中文行保持中文念）；Say 留空 = 走混排。
var mixVariants = map[string][]podcast.Line{
	// A：按行分语言——中文行纯中文（词也按中文念），日语读音靠独立示范行
	"mixA": {
		{Role: podcast.RoleA, Text: "中国人这个词，自我介绍的时候最常用。", Say: "中国人这个词，自我介绍的时候最常用。"},
		{Role: podcast.RoleA, Text: "听——", Say: "听——"},
		{Role: podcast.RoleB, Text: "李さんは中国人です。"},
		{Role: podcast.RoleB, Text: "意思是：小李是中国人。", Say: "意思是：小李是中国人。"},
		{Role: podcast.RoleA, Text: "注意，说自己的国籍就用这句。", Say: "注意，说自己的国籍就用这句。"},
	},
	// B：句内拆成同一个人连续两行——中文半句 + 日语半句，听觉上像一句里切换
	"mixB": {
		{Role: podcast.RoleA, Text: "小李是——", Say: "小李是——"},
		{Role: podcast.RoleA, Text: "ちゅうごくじん。"},
		{Role: podcast.RoleA, Text: "自我介绍的时候，最常用这句。", Say: "自我介绍的时候，最常用这句。"},
		{Role: podcast.RoleB, Text: "对，先说人，再说国籍。", Say: "对，先说人，再说国籍。"},
	},
	// C：现在的句内混排（中文行里夹假名）——已知中文部分会被带成日语腔，留作对照
	"mixC": {
		{Role: podcast.RoleA, Text: "小李是中国人。自我介绍的时候最常用这句。"},
		{Role: podcast.RoleB, Text: "对，先说人，再说国籍。", Say: "对，先说人，再说国籍。"},
	},
}

// demoInput 为 LLM 出稿模式的演示原料。用「中国人/山田/日本人」这类中日同形词：
// 它们正是 2026-11-04 语言串味报障的词类（中文行被假名化后整行念成日语），留作回归夹具。
var demoInput = podcast.Input{
	Title: "国籍和人名",
	Words: []podcast.WordBrief{
		{Surface: "中国人", Reading: "ちゅうごくじん", Meaning: "中国人", Sentences: []podcast.SentenceBrief{
			{Text: "李さんは中国人ですか。", Translation: "小李是中国人吗？"},
		}},
		{Surface: "山田", Reading: "やまだ", Meaning: "山田（日本姓氏）", Sentences: []podcast.SentenceBrief{
			{Text: "山田さんは先生です。", Translation: "山田是老师。"},
		}},
		{Surface: "日本人", Reading: "にほんじん", Meaning: "日本人", Sentences: []podcast.SentenceBrief{
			{Text: "彼は日本人ではありません。", Translation: "他不是日本人。"},
		}},
	},
}

func main() {
	_ = envcfg.Load(".env.dev", ".env")
	key := os.Getenv("DOUBAO_TTS_API_KEY")
	if key == "" {
		fmt.Fprintln(os.Stderr, "DOUBAO_TTS_API_KEY 未设置")
		os.Exit(1)
	}
	out := "G:/TingShuo/podcast-samples/go-v13.mp3"
	if len(os.Args) > 1 {
		out = os.Args[1]
	}

	script := fixedScript
	if v := os.Getenv("POD_FIX"); v != "" {
		vs, ok := mixVariants[v]
		if !ok {
			fmt.Fprintln(os.Stderr, "POD_FIX 未知取值: "+v)
			os.Exit(1)
		}
		script = vs
	}
	if os.Getenv("POD_SCRIPT_LLM") == "1" {
		ctx0 := context.Background()
		t0 := time.Now()
		lines, err := podcast.BuildScript(ctx0, demoInput, func(p podcast.Progress) {
			fmt.Printf("  [%s] 写稿 %d/%d 段完成（刚写完 %s）\n",
				time.Since(t0).Round(time.Second), p.Seg, p.Total, p.Label)
		})
		if err != nil {
			fmt.Fprintln(os.Stderr, "LLM 脚本失败:", err)
			os.Exit(1)
		}
		script = lines
		podcast.FillSay(script, demoInput.Words) // 与 generate 同一道语言把关，样片所见即所听
		chars := 0
		for _, l := range script {
			chars += len([]rune(l.Text))
		}
		fmt.Printf("脚本 %d 行 %d 字 约 %d 分钟（按实测 309 字/分）耗时 %s\n",
			len(script), chars, chars/309, time.Since(t0).Round(time.Second))
		for i, l := range script {
			say := ""
			if l.Say != "" && l.Say != l.Text {
				say = "   ⇒合成: " + l.Say
			}
			fmt.Printf("%3d %s %s%s\n", i+1, l.Role, l.Text, say)
		}
		if os.Getenv("POD_SCRIPT_ONLY") == "1" {
			return
		}
	}

	lines, err := podcast.SessionLines(script)
	if err != nil {
		fmt.Fprintln(os.Stderr, "脚本校验失败:", err)
		os.Exit(1)
	}
	client := doubaopod.New(key)
	ctx, cancel := context.WithTimeout(context.Background(), 1800*time.Second)
	defer cancel()

	for i, l := range lines {
		fmt.Printf("合成 %2d %s: %s\n", i+1, l.Speaker[:12], l.Text)
	}

	rounds, err := client.Session(ctx, [2]string{doubaopod.VoiceSisi, doubaopod.VoiceXudong}, lines, nil, nil)
	if err != nil {
		fmt.Fprintln(os.Stderr, "会话失败:", err)
		os.Exit(1)
	}
	audio, tl, err := podcast.AssembleRounds(rounds, script)
	if err != nil {
		fmt.Fprintln(os.Stderr, "装配失败:", err)
		os.Exit(1)
	}
	if err := os.WriteFile(out, audio, 0o644); err != nil {
		fmt.Fprintln(os.Stderr, "写 mp3 失败:", err)
		os.Exit(1)
	}
	tj, _ := json.MarshalIndent(tl, "", "  ")
	if err := os.WriteFile(out+".timeline.json", tj, 0o644); err != nil {
		fmt.Fprintln(os.Stderr, "写时间轴失败:", err)
		os.Exit(1)
	}
	if err := os.WriteFile(out+".srt", []byte(podcast.SRT(tl)), 0o644); err != nil {
		fmt.Fprintln(os.Stderr, "写 SRT 失败:", err)
		os.Exit(1)
	}
	fmt.Printf("OK rounds=%d duration_ms=%d out=%s\n", len(rounds), tl[len(tl)-1].EndMS, out)
}
