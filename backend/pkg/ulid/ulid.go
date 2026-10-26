// Package ulid 提供 ULID 生成与解析（§5.2：ID 统一用 ULID，时间有序，便于同步游标）。
//
// ULID：128 bit = 48 bit Unix 毫秒时间戳 + 80 bit crypto/rand 随机，
// 编码为 26 字符 Crockford Base32（大写）。同一毫秒内冲突概率 2^-80 量级，
// 主键冲突由插入失败重试兜底，不实现单调递增。
package ulid

import (
	"crypto/rand"
	"errors"
	"math/big"
	"strings"
	"time"
)

// alphabet 是 Crockford Base32 字符集（无 I、L、O、U）。
const alphabet = "0123456789ABCDEFGHJKMNPQRSTVWXYZ"

const encodedLen = 26

var (
	radix  = big.NewInt(32)
	errLen = errors.New("ulid: 长度必须为 26 字符")
	errChr = errors.New("ulid: 含非法字符")
)

// MustNew 生成 ULID，随机源失败时 panic（crypto/rand 在常规平台不会失败）。
func MustNew() string {
	s, err := New()
	if err != nil {
		panic(err)
	}
	return s
}

// New 生成 26 字符大写 ULID。
func New() (string, error) {
	var b [16]byte
	ts := uint64(time.Now().UnixMilli())
	b[0] = byte(ts >> 40)
	b[1] = byte(ts >> 32)
	b[2] = byte(ts >> 24)
	b[3] = byte(ts >> 16)
	b[4] = byte(ts >> 8)
	b[5] = byte(ts)
	if _, err := rand.Read(b[6:]); err != nil {
		return "", err
	}

	// 128bit ÷ 5bit/字符 = 25.6 → 26 字符（前导 bit 补 0，故 ULID 以 "01" 开头直至 2286 年）
	n := new(big.Int).SetBytes(b[:])
	out := make([]byte, encodedLen)
	mod := new(big.Int)
	for i := encodedLen - 1; i >= 0; i-- {
		n.DivMod(n, radix, mod)
		out[i] = alphabet[mod.Int64()]
	}
	return string(out), nil
}

// Time 校验整个 ID 的合法性，并从前 10 字符解码出时间戳（同步游标按时间序比较时使用）。
func Time(id string) (time.Time, error) {
	if len(id) != encodedLen {
		return time.Time{}, errLen
	}
	var ts uint64
	for i := 0; i < encodedLen; i++ {
		v := strings.IndexByte(alphabet, id[i])
		if v < 0 {
			return time.Time{}, errChr
		}
		if i < 10 {
			ts = ts<<5 | uint64(v)
		}
	}
	return time.UnixMilli(int64(ts)), nil
}
