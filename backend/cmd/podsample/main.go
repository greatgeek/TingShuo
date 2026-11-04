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

// demoInput 为 LLM 出稿模式的演示原料（一个目录的典型词，每词带自己的例句）。
var demoInput = podcast.Input{
	Title: "钱和生活",
	Words: []podcast.WordBrief{
		{Surface: "食費", Reading: "しょくひ", Meaning: "伙食费，吃饭花的钱", Sentences: []podcast.SentenceBrief{
			{Text: "今月は外食が多かったので、食費が増えました。", Translation: "这个月在外吃饭比较多，所以伙食费增加了。"},
		}},
		{Surface: "外食", Reading: "がいしょく", Meaning: "在外吃饭，下馆子", Sentences: []podcast.SentenceBrief{
			{Text: "週末は家族と外食します。", Translation: "周末和家人出去吃饭。"},
		}},
		{Surface: "財布", Reading: "さいふ", Meaning: "钱包", Sentences: []podcast.SentenceBrief{
			{Text: "財布の中に千円しか残っていません。", Translation: "钱包里只剩一千日元了。"},
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
		chars := 0
		for _, l := range script {
			chars += len([]rune(l.Text))
		}
		fmt.Printf("脚本 %d 行 %d 字 约 %d 分钟（按实测 309 字/分）耗时 %s\n",
			len(script), chars, chars/309, time.Since(t0).Round(time.Second))
		for i, l := range script {
			fmt.Printf("%3d %s %s\n", i+1, l.Role, l.Text)
		}
		if os.Getenv("POD_SCRIPT_ONLY") == "1" {
			return
		}
	}

	for i := range script {
		if script[i].Say == "" {
			script[i].Say = podcast.Kanaize(script[i].Text, demoInput.Words)
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
