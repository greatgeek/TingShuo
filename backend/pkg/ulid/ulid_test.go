package ulid

import (
	"strings"
	"testing"
	"time"
)

func TestNewFormat(t *testing.T) {
	for i := 0; i < 100; i++ {
		id := MustNew()
		if len(id) != 26 {
			t.Fatalf("长度 = %d, 期望 26：%s", len(id), id)
		}
		if id != strings.ToUpper(id) {
			t.Fatalf("必须为大写：%s", id)
		}
		for _, c := range id {
			if !strings.ContainsRune(alphabet, c) {
				t.Fatalf("非法字符 %q：%s", c, id)
			}
		}
	}
}

func TestTimeRoundtrip(t *testing.T) {
	before := time.Now().UnixMilli()
	id := MustNew()
	after := time.Now().UnixMilli()
	ts, err := Time(id)
	if err != nil {
		t.Fatal(err)
	}
	if ts.UnixMilli() < before || ts.UnixMilli() > after {
		t.Fatalf("解码时间 %v 超出生成窗口 [%v, %v]", ts, before, after)
	}
}

func TestUniqueness(t *testing.T) {
	seen := make(map[string]struct{}, 10000)
	for i := 0; i < 10000; i++ {
		id := MustNew()
		if _, dup := seen[id]; dup {
			t.Fatalf("10000 次内出现重复：%s", id)
		}
		seen[id] = struct{}{}
	}
}

func TestTimeInvalid(t *testing.T) {
	if _, err := Time("01JAX"); err == nil {
		t.Fatal("短 ID 应报错")
	}
	if _, err := Time("01JAXXXXXXXXXXXXXXXXXXXXIL"); err == nil {
		t.Fatal("含 I/L 非法字符应报错")
	}
}
