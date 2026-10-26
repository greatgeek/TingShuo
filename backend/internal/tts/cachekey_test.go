package tts

import "testing"

func TestCacheKeyStable(t *testing.T) {
	p := Params{Voice: "Nanami", Rate: 1.0, Style: "", EngineVer: "azure-2024"}
	k1 := CacheKey("これ、お願いします。", p)
	k2 := CacheKey("これ、お願いします。", p)
	if k1 != k2 {
		t.Fatalf("同句同参数应命中同一缓存键：%s vs %s", k1, k2)
	}
}

func TestCacheKeyDiffersByParams(t *testing.T) {
	s := "全部で480円になります。"
	base := Params{Voice: "Nanami", Rate: 1.0, Style: "", EngineVer: "v1"}

	if CacheKey(s, base) == CacheKey(s, Params{Voice: "Keita", Rate: 1.0, Style: "", EngineVer: "v1"}) {
		t.Error("换音色必须产生新缓存键")
	}
	if CacheKey(s, base) == CacheKey(s, Params{Voice: "Nanami", Rate: 0.85, Style: "", EngineVer: "v1"}) {
		t.Error("换语速必须产生新缓存键")
	}
	if CacheKey(s, base) == CacheKey(s, Params{Voice: "Nanami", Rate: 1.0, Style: "cheerful", EngineVer: "v1"}) {
		t.Error("换风格必须产生新缓存键")
	}
	if CacheKey(s, base) == CacheKey(s, Params{Voice: "Nanami", Rate: 1.0, Style: "", EngineVer: "v2"}) {
		t.Error("换引擎版本必须产生新缓存键")
	}
}

func TestCacheKeyNormalizesWhitespace(t *testing.T) {
	p := Params{Voice: "Keita", Rate: 1.15, Style: "", EngineVer: "v1"}
	// 首尾空白 + 句中连续空白，归一后应与规范写法命中同一键
	a := CacheKey("  これ、  お願いします。  ", p)
	b := CacheKey("これ、 お願いします。", p)
	if a != b {
		t.Fatalf("仅空白差异的句子应归一为同一缓存键：%s vs %s", a, b)
	}
}

func TestRateFormatEquivalence(t *testing.T) {
	if formatRate(1) != formatRate(1.0) {
		t.Fatal("1 与 1.0 必须格式化为相同字符串")
	}
	if formatRate(0.85) != "0.85" || formatRate(1.15) != "1.15" {
		t.Fatalf("三档语速格式化异常: %s / %s", formatRate(0.85), formatRate(1.15))
	}
}
