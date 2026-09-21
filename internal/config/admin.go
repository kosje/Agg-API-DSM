// Package config 的管理端口令。
//
// 与下游 API Key 分开：Key 是给程序用的（高熵随机串，SHA-256 足够），
// 口令是给人用的（低熵、可能被猜到，必须用慢散列）。
//
// 用 PBKDF2-HMAC-SHA256 而不是 bcrypt：两者对「人选口令」都是合格的选择，
// 而 PBKDF2 标准库就有（crypto/hmac + crypto/sha256），不必为此引
// golang.org/x/crypto —— 本项目的依赖策略是只留标准库无法替代的。
package config

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// PBKDF2 参数。
//
// 迭代次数取 210000：这是 OWASP 对 PBKDF2-HMAC-SHA256 的当前建议值。
// 调高会让登录变慢（每次要算这么久），调低会削弱抗暴力破解能力 ——
// 管理端登录不是高频操作，慢一点无所谓。
const (
	pbkdf2Iterations = 210000
	pbkdf2KeyLen     = 32
	saltLen          = 16
)

// MinPasswordLen 是管理端口令的最小长度。
//
// 12 位：比常见的 8 位强得多，又不至于让用户为了记口令而写在便签上。
const MinPasswordLen = 12

// AdminAuth 是管理端口令的散列记录。
type AdminAuth struct {
	// Hash 是 "pbkdf2-sha256$<iterations>$<salt-b64>$<key-b64>" 形式的自描述串。
	// 把参数一起存进去，将来调迭代次数时旧口令仍然可校验。
	Hash      string    `json:"hash"`
	UpdatedAt time.Time `json:"updated_at"`
}

// ErrNoAdminPassword 尚未设置管理端口令。
var ErrNoAdminPassword = errors.New("尚未设置管理端口令")

// ErrBadPassword 口令不正确。
var ErrBadPassword = errors.New("口令不正确")

// pbkdf2 计算派生密钥。标准库没有现成实现，这里按 RFC 8018 写一遍。
func pbkdf2(password, salt []byte, iter, keyLen int) []byte {
	prf := hmac.New(sha256.New, password)
	hashLen := prf.Size()
	blocks := (keyLen + hashLen - 1) / hashLen

	out := make([]byte, 0, blocks*hashLen)
	buf := make([]byte, 4)
	for block := 1; block <= blocks; block++ {
		prf.Reset()
		prf.Write(salt)
		buf[0] = byte(block >> 24)
		buf[1] = byte(block >> 16)
		buf[2] = byte(block >> 8)
		buf[3] = byte(block)
		prf.Write(buf)
		u := prf.Sum(nil)

		t := make([]byte, len(u))
		copy(t, u)
		for i := 1; i < iter; i++ {
			prf.Reset()
			prf.Write(u)
			u = prf.Sum(u[:0])
			for j := range t {
				t[j] ^= u[j]
			}
		}
		out = append(out, t...)
	}
	return out[:keyLen]
}

func hashPassword(plain string) (string, error) {
	salt := make([]byte, saltLen)
	if _, err := rand.Read(salt); err != nil {
		return "", err
	}
	key := pbkdf2([]byte(plain), salt, pbkdf2Iterations, pbkdf2KeyLen)
	return fmt.Sprintf("pbkdf2-sha256$%d$%s$%s",
		pbkdf2Iterations,
		base64.RawStdEncoding.EncodeToString(salt),
		base64.RawStdEncoding.EncodeToString(key)), nil
}

func verifyPassword(plain, encoded string) bool {
	parts := strings.Split(encoded, "$")
	if len(parts) != 4 || parts[0] != "pbkdf2-sha256" {
		return false
	}
	iter, err := strconv.Atoi(parts[1])
	if err != nil || iter <= 0 {
		return false
	}
	salt, err := base64.RawStdEncoding.DecodeString(parts[2])
	if err != nil {
		return false
	}
	want, err := base64.RawStdEncoding.DecodeString(parts[3])
	if err != nil {
		return false
	}
	got := pbkdf2([]byte(plain), salt, iter, len(want))
	return subtle.ConstantTimeCompare(got, want) == 1
}

// HasAdminPassword 报告是否已设置管理端口令。
func (s *Store) HasAdminPassword() bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.Admin != nil && s.Admin.Hash != ""
}

// SetAdminPassword 设置或修改管理端口令。
func (s *Store) SetAdminPassword(plain string) error {
	if len([]rune(plain)) < MinPasswordLen {
		return fmt.Errorf("口令至少 %d 位", MinPasswordLen)
	}
	h, err := hashPassword(plain)
	if err != nil {
		return err
	}
	s.mu.Lock()
	s.Admin = &AdminAuth{Hash: h, UpdatedAt: time.Now().UTC()}
	s.mu.Unlock()
	return s.save()
}

// CheckAdminPassword 校验管理端口令。
func (s *Store) CheckAdminPassword(plain string) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.Admin == nil {
		return false
	}
	return verifyPassword(plain, s.Admin.Hash)
}
