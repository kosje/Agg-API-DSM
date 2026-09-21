// Package uid 生成随机 ID。
//
// 刻意不用 github.com/google/uuid：标准库 crypto/rand 足够，
// 少一个依赖就少一份供应链风险。上游 M365-Copilot2API 为此多引了一个包，
// 本项目把依赖压到最小，这一类工具函数自己写。
package uid

import (
	"crypto/rand"
	"encoding/hex"
)

// New 生成一个 RFC 4122 形状的随机 ID（32 位十六进制 + 4 个连字符）。
//
// 版本位与变体位按规范设置，让它长得像标准 UUID —— 便于用户复制到
// 别处使用，也避免某些客户端把它当成非法字符串拒绝。
func New() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return format(b), nil
}

// MustNew 与 New 相同，但随机源失败时 panic。
//
// 随机源不可用意味着系统已经不正常，继续运行只会产生更隐蔽的故障。
// 用于那些「调用点无法优雅处理错误」的地方。
func MustNew() string {
	s, err := New()
	if err != nil {
		panic("uid: 随机源不可用: " + err.Error())
	}
	return s
}

func format(b [16]byte) string {
	var dst [36]byte
	hex.Encode(dst[0:8], b[0:4])
	dst[8] = '-'
	hex.Encode(dst[9:13], b[4:6])
	dst[13] = '-'
	hex.Encode(dst[14:18], b[6:8])
	dst[18] = '-'
	hex.Encode(dst[19:23], b[8:10])
	dst[23] = '-'
	hex.Encode(dst[24:36], b[10:16])
	return string(dst[:])
}
