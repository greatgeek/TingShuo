// Package passhash 提供密码哈希（bcrypt，§4 安全：密码 bcrypt/argon2）。
package passhash

import "golang.org/x/crypto/bcrypt"

// Hash 生成 bcrypt 哈希（cost 10：单次 ~50ms，登录频率下无压力）。
func Hash(plain string) (string, error) {
	b, err := bcrypt.GenerateFromPassword([]byte(plain), 10)
	return string(b), err
}

// Verify 校验密码；不匹配返回 false（不区分「无此用户」与「密码错」，防枚举）。
func Verify(hash, plain string) bool {
	return bcrypt.CompareHashAndPassword([]byte(hash), []byte(plain)) == nil
}
