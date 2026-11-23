package wordstudy

import (
	"encoding/json"
	"testing"
)

func ruby(t *testing.T, pairs [][2]string) json.RawMessage {
	t.Helper()
	out := []any{}
	for _, p := range pairs {
		var r *string
		if p[1] != "" {
			v := p[1]
			r = &v
		}
		out = append(out, [2]*string{&p[0], r})
	}
	b, err := json.Marshal(out)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// 目标词跨两个 token（コーヒー 拆成 コー/ヒー）也要整体空白，且空白不带读音。
func TestBlankTokensMergesOverlap(t *testing.T) {
	raw := ruby(t, [][2]string{{"彼", "かれ"}, {"は", ""}, {"コー", ""}, {"ヒー", ""}, {"を", ""}, {"飲", "の"}, {"む", ""}})
	got := blankTokens(raw, "彼はコーヒーを飲む", "コーヒー", "")
	if got == nil {
		t.Fatal("nil tokens")
	}
	blanks, texts := 0, ""
	for _, tk := range got {
		if tk.Blank {
			blanks++
			if tk.Reading != nil {
				t.Errorf("空白 token 不应带读音: %v", *tk.Reading)
			}
			continue
		}
		texts += tk.Text
		if tk.Text == "コー" || tk.Text == "ヒー" {
			t.Errorf("目标词残片漏出: %q", tk.Text)
		}
	}
	if blanks != 1 {
		t.Errorf("空白 token 数 = %d, 想 1", blanks)
	}
	if texts != "彼はを飲む" {
		t.Errorf("其余 token 拼接 = %q", texts)
	}
}

// 目标词是更大词元的一部分：词外文字必须保留，读音按词读音切给残留文字，
// 否则揭晓时整块被词面替换，屏幕句子缺字、和整句音频对不上（2026-11-16 报障）。
func TestBlankTokensKeepsOutsideText(t *testing.T) {
	raw := ruby(t, [][2]string{{"言語学習", "げんごがくしゅう"}, {"に", ""}, {"も", ""}, {"便利", "べんり"}, {"です", ""}, {"。", ""}})
	got := blankTokens(raw, "言語学習にも便利です。", "学習", "がくしゅう")
	if got == nil {
		t.Fatal("nil tokens")
	}
	if len(got) != 7 {
		t.Fatalf("token 数 = %d, 想 7: %+v", len(got), got)
	}
	if got[0].Text != "言語" || got[0].Reading == nil || *got[0].Reading != "げんご" {
		t.Errorf("前缀 token = %+v, 想 言語/げんご", got[0])
	}
	if !got[1].Blank {
		t.Errorf("第二个 token 应为空白: %+v", got[1])
	}
	texts := ""
	for _, tk := range got {
		if !tk.Blank {
			texts += tk.Text
		}
	}
	if texts != "言語にも便利です。" {
		t.Errorf("词外文字拼接 = %q", texts)
	}

	// 词在词元头部：余下部分照常显示，读音切出后缀
	raw2 := ruby(t, [][2]string{{"李さん", "りさん"}, {"は", ""}, {"中国人", "ちゅうごくじん"}, {"です", ""}})
	got2 := blankTokens(raw2, "李さんは中国人です", "さん", "さん")
	if got2 == nil {
		t.Fatal("nil tokens (suffix)")
	}
	if got2[0].Text != "李" || got2[0].Reading == nil || *got2[0].Reading != "り" {
		t.Errorf("前缀 token = %+v, 想 李/り", got2[0])
	}
	if !got2[1].Blank || got2[2].Text != "は" {
		t.Errorf("token 顺序 = %+v", got2)
	}
}

func TestBlankTokensMissingSurface(t *testing.T) {
	raw := ruby(t, [][2]string{{"今日", "きょう"}, {"は", ""}})
	if got := blankTokens(raw, "今日は晴天", "コーヒー", ""); got != nil {
		t.Errorf("词面不在句中应返回 nil, 得 %v", got)
	}
}

// 词面先出现在别的词里（味噌汁 的 味、花瓶 的 花）时，要挖独立词那一处，
// 否则音频在空白处念的是复合词读音，和答案对不上（2026-11-20 报障）。
func TestBlankTokensSkipsCompoundOccurrence(t *testing.T) {
	raw := ruby(t, [][2]string{{"この", ""}, {"味噌汁", "みそしる"}, {"の", ""}, {"味", "あじ"}, {"が", ""}, {"濃い", "こい"}, {"です", ""}, {"ね", ""}, {"。", ""}})
	got := blankTokens(raw, "この味噌汁の味が濃いですね。", "味", "あじ")
	if got == nil {
		t.Fatal("nil tokens")
	}
	blanks, texts := 0, ""
	for _, tk := range got {
		if tk.Blank {
			blanks++
			continue
		}
		texts += tk.Text
	}
	if blanks != 1 || texts != "この味噌汁のが濃いですね。" {
		t.Fatalf("blank=%d texts=%q tokens=%+v", blanks, texts, got)
	}

	raw2 := ruby(t, [][2]string{{"花瓶", "かびん"}, {"に", ""}, {"花", "はな"}, {"を", ""}, {"飾り", "かざり"}, {"ます", ""}, {"。", ""}})
	got2 := blankTokens(raw2, "花瓶に花を飾ります。", "花", "はな")
	if got2 == nil {
		t.Fatal("nil tokens (花)")
	}
	texts = ""
	for _, tk := range got2 {
		if !tk.Blank {
			texts += tk.Text
		}
	}
	if texts != "花瓶にを飾ります。" {
		t.Fatalf("花 句拼接 = %q", texts)
	}
}

// 全句只有读音对不上的复合词片段（体/からだ 出现在 体調/たいちょう）→ 不出挖空题。
func TestBlankTokensRejectsReadingMismatch(t *testing.T) {
	raw := ruby(t, [][2]string{{"今日", "きょう"}, {"は", ""}, {"体調", "たいちょう"}, {"が", ""}, {"悪い", "わるい"}, {"です", ""}, {"ね", ""}, {"。", ""}})
	if got := blankTokens(raw, "今日は体調が悪いですね。", "体", "からだ"); got != nil {
		t.Errorf("读音对不上应返回 nil, 得 %+v", got)
	}
}

// 读音写法归一化：多读音（いき・ゆき）与音调符号（まね ⓪）不应误杀正常挖空。
func TestBlankTokensReadingVariants(t *testing.T) {
	raw := ruby(t, [][2]string{{"会社", "かいしゃ"}, {"へ", ""}, {"行きます", "いきます"}, {"。", ""}})
	if got := blankTokens(raw, "会社へ行きます。", "行き", "いき・ゆき"); got == nil {
		t.Error("いき・ゆき 应能匹配 いきます")
	}
	raw2 := ruby(t, [][2]string{{"弟", "おとうと"}, {"は", ""}, {"真似して", "まねして"}, {"います", ""}, {"。", ""}})
	if got := blankTokens(raw2, "弟は真似しています。", "真似", "まね ⓪"); got == nil {
		t.Error("まね ⓪ 应能匹配 まねして")
	}
}

// 跨句挑选句子的依据：独立词元(3) > 读音可切出(2) > 读音缺失(1) > 对不上(0)。
func TestBlankTokensScored(t *testing.T) {
	// 复合词在前、独立词在后：仍挖独立词，保证词义和语音都是目标词
	raw := ruby(t, [][2]string{{"本番", "ほんばん"}, {"と", ""}, {"この", ""}, {"本", "ほん"}})
	tokens, score := blankTokensScored(raw, "本番とこの本", "本", "ほん")
	if score != scoreExact || tokens == nil {
		t.Fatalf("score = %d, 想 %d: %+v", score, scoreExact, tokens)
	}
	if tokens[0].Text != "本番" || tokens[0].Blank {
		t.Errorf("本番 的 本 不应被挖: %+v", tokens[0])
	}

	// 只在复合词里出现且读音切得出：兼容分，只能兜底用
	raw2 := ruby(t, [][2]string{{"日本", "にほん"}, {"です", ""}})
	if _, sc := blankTokensScored(raw2, "日本です", "本", "ほん"); sc != scoreCompat {
		t.Errorf("日本/にほん score = %d, 想 %d", sc, scoreCompat)
	}
	if _, sc := blankTokensScored(raw2, "日本です", "本", "にっぽん"); sc != scoreBad {
		t.Errorf("读音对不上应 scoreBad, 得 %d", sc)
	}
}

// 句式条目（〜さん/…）不进听力通道：念出来不明所以（2026-11-06 报障）
func TestPatternWord(t *testing.T) {
	for _, s := range []string{"〜さんのおかげです", "〜てください", "…でもいい"} {
		if !patternWord(s) {
			t.Errorf("%q 应判为句式条目", s)
		}
	}
	for _, s := range []string{"切符", "増える", "何曜日"} {
		if patternWord(s) {
			t.Errorf("%q 不应判为句式条目", s)
		}
	}
}

func TestBlurHint(t *testing.T) {
	tr, mg := "他每天早上喝咖啡。", "咖啡"
	h := blurHint(&tr, &mg)
	if h == nil || *h != "他每天早上喝○○。" {
		t.Fatalf("hint = %v", h)
	}
	// 翻译里不含词义 → 不给提示，宁缺勿泄
	tr2 := "他起得很早。"
	if h2 := blurHint(&tr2, &mg); h2 != nil {
		t.Errorf("不含词义应为 nil, 得 %q", *h2)
	}
	if blurHint(nil, &mg) != nil || blurHint(&tr, nil) != nil {
		t.Error("缺翻译或缺词义应为 nil")
	}
}

// 一场测试内四种题型都要出现，再按本轮循环，避免整场固定一种模式。
func TestMixTypeInterleaves(t *testing.T) {
	want := []quizType{qPickMean, qClozePick, qHearMean, qClozeType}
	seen := map[quizType]bool{}
	for i := 0; i < 8; i++ {
		if got := mixType(i); got != want[i%4] {
			t.Fatalf("mixType(%d) = %s, 想 %s", i, got, want[i%4])
		}
		seen[mixType(i)] = true
	}
	for _, typ := range want {
		if !seen[typ] {
			t.Errorf("交错题型缺少 %s", typ)
		}
	}
}

// 原声片段读音转注音：片假名转平假名、空读音不挂 rt，拼接不符则整条放弃。
func TestClipRuby(t *testing.T) {
	raw := json.RawMessage(`[{"s":"彼","r":"カレ"},{"s":"は","r":"ワ"},{"s":"学生","r":"ガクセイ"},{"s":"です","r":"デス"}]`)
	got := clipRuby(raw, "彼は学生です")
	if got == nil {
		t.Fatal("clipRuby 返回 nil")
	}
	var out [][2]*string
	if err := json.Unmarshal(got, &out); err != nil {
		t.Fatal(err)
	}
	if len(out) != 4 {
		t.Fatalf("token 数 = %d, 想 4", len(out))
	}
	if out[0][1] == nil || *out[0][1] != "かれ" {
		t.Errorf("彼 读音 = %v, 想 かれ", out[0][1])
	}
	if out[1][1] == nil || *out[1][1] != "わ" {
		t.Errorf("は 读音 = %v, 想 わ", out[1][1])
	}
	if out[2][1] == nil || *out[2][1] != "がくせい" {
		t.Errorf("学生 读音 = %v, 想 がくせい", out[2][1])
	}
	// 拼接与正文不一致：整条不出题，避免注音错位
	if clipRuby(raw, "彼女は学生です") != nil {
		t.Error("正文对不上应返回 nil")
	}
	raw2 := json.RawMessage(`[{"s":"コーヒー","r":"コーヒー"}]`)
	got2 := clipRuby(raw2, "コーヒー")
	var out2 [][2]*string
	if err := json.Unmarshal(got2, &out2); err != nil || len(out2) != 1 {
		t.Fatalf("out2 = %v, err = %v", out2, err)
	}
	if out2[0][1] != nil {
		t.Errorf("读音等于词面不应挂 rt, 得 %q", *out2[0][1])
	}
}

func TestKanaReading(t *testing.T) {
	cases := map[string]string{
		"カレ":    "かれ",
		"デス":    "です",
		"コーヒー":  "こーひー",
		"かれ":    "かれ",
		"":      "",
		"abc":   "",
		"漢字":    "",
		"ガク セイ": "",
	}
	for in, want := range cases {
		if got := kanaReading(in); got != want {
			t.Errorf("kanaReading(%q) = %q, 想 %q", in, got, want)
		}
	}
}
