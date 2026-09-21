// Package config 的下游密钥管理。
//
// 下游用 API Key 访问本网关，与上游账号凭据是两回事：
// 上游凭据是「我们怎么连别人」，下游密钥是「别人怎么连我们」。
package config

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"strings"
	"time"
)

// KeyPrefix 是所有下游密钥的固定前缀。
//
// 加前缀有两个用处：用户在客户端里一眼能认出这是本网关的密钥；
// 泄漏时也方便在日志/代码里做特征扫描。
const KeyPrefix = "agg_"

// Key 是一条下游密钥。**只存散列，不存明文。**
type Key struct {
	ID        string    `json:"id"`
	Name      string    `json:"name"`
	Prefix    string    `json:"prefix"`
	Hash      string    `json:"hash"`
	Enabled   bool      `json:"enabled"`
	CreatedAt time.Time `json:"created_at"`
	// LastUsedAt 便于用户判断「这条密钥还在用吗」。
	LastUsedAt time.Time `json:"last_used_at,omitempty"`
}

// ErrKeyNotFound 找不到密钥。
var ErrKeyNotFound = errors.New("密钥不存在或已停用")

// hashKey 计算密钥散列。
//
// 用 SHA-256 而不是 bcrypt，是刻意的选择：
// 密钥是 32 字节随机串（256 位熵），不存在字典攻击空间，
// 慢散列在这里只是白白增加每次请求的延迟。bcrypt 是给「人选的低熵口令」用的。
func hashKey(plain string) string {
	sum := sha256.Sum256([]byte(plain))
	return hex.EncodeToString(sum[:])
}

// Keys 返回全部下游密钥（不含明文，本来也没存）。
func (s *Store) Keys() []Key {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]Key, len(s.APIKeys))
	copy(out, s.APIKeys)
	return out
}

// CreateKey 生成一条新密钥，返回明文（仅此一次可见）。
func (s *Store) CreateKey(name string) (Key, string, error) {
	var raw [32]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return Key{}, "", err
	}
	plain := KeyPrefix + hex.EncodeToString(raw[:])

	id, err := newID()
	if err != nil {
		return Key{}, "", err
	}
	k := Key{
		ID:        id,
		Name:      strings.TrimSpace(name),
		Prefix:    plain[:len(KeyPrefix)+8],
		Hash:      hashKey(plain),
		Enabled:   true,
		CreatedAt: time.Now().UTC(),
	}
	if k.Name == "" {
		k.Name = "未命名"
	}

	s.mu.Lock()
	s.APIKeys = append(s.APIKeys, k)
	s.mu.Unlock()

	if err := s.save(); err != nil {
		return Key{}, "", err
	}
	return k, plain, nil
}

// DeleteKey 删除一条密钥。
func (s *Store) DeleteKey(id string) error {
	s.mu.Lock()
	idx := -1
	for i := range s.APIKeys {
		if s.APIKeys[i].ID == id {
			idx = i
			break
		}
	}
	if idx < 0 {
		s.mu.Unlock()
		return ErrKeyNotFound
	}
	s.APIKeys = append(s.APIKeys[:idx], s.APIKeys[idx+1:]...)
	s.mu.Unlock()
	return s.save()
}

// VerifyKey 校验下游密钥。返回命中的密钥 ID。
//
// 用常量时间比较：虽然散列值本身不是秘密，但避免比较提前返回
// 可以少一个可供计时的侧信道，成本近乎为零。
func (s *Store) VerifyKey(plain string) (string, bool) {
	if !strings.HasPrefix(plain, KeyPrefix) {
		return "", false
	}
	want := hashKey(plain)

	s.mu.RLock()
	defer s.mu.RUnlock()
	for i := range s.APIKeys {
		k := &s.APIKeys[i]
		if !k.Enabled {
			continue
		}
		if subtle.ConstantTimeCompare([]byte(k.Hash), []byte(want)) == 1 {
			return k.ID, true
		}
	}
	return "", false
}

// TouchKey 更新密钥的最后使用时间。
//
// 不落盘：这个字段只用于控制台展示，为了它每次请求都写一次文件不划算。
// 进程重启后归零是可接受的。
func (s *Store) TouchKey(id string) {
	now := time.Now().UTC()
	s.mu.Lock()
	for i := range s.APIKeys {
		if s.APIKeys[i].ID == id {
			s.APIKeys[i].LastUsedAt = now
			break
		}
	}
	s.mu.Unlock()
}

// HasKeys 报告是否已配置下游密钥。
//
// 网关据此决定「是否允许无密钥访问」：一条都没有时放行本机调试，
// 一旦配了就必须带密钥 —— 否则用户配了密钥却发现没生效。
func (s *Store) HasKeys() bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for i := range s.APIKeys {
		if s.APIKeys[i].Enabled {
			return true
		}
	}
	return false
}

// KeyCount 返回启用中的密钥数量。
func (s *Store) KeyCount() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	n := 0
	for i := range s.APIKeys {
		if s.APIKeys[i].Enabled {
			n++
		}
	}
	return n
}
