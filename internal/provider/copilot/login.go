// Package copilot 的账号授权流程。
//
// # 为什么只有「浏览器 PKCE + 手动粘贴」这一种
//
// 上游 M365-Copilot2API 提供了设备码登录（推荐）与手动粘贴（备用）两种。
// 本项目**刻意只保留后者**，两个原因：
//
//  1. 用户明确要求去掉设备码流程（用不上）；
//  2. 更根本的是，微软自 **2026 年 7 月 1 日**起对所有新建 Entra 租户
//     在安全默认值下阻止设备码流程（原因是设备码钓鱼），现有租户的管理员
//     也可随时关闭。也就是说设备码正在退场，而手动粘贴是唯一退路。
//
// # 手动粘贴为什么不需要自己起回调服务
//
// 重定向地址用的是微软自家的
//
//	https://login.microsoftonline.com/common/oauth2/nativeclient
//
// 这是这个第一方客户端的公开客户端回调页。用户登录后浏览器会跳到它，
// 页面本身是空白的，但**地址栏里带着 code** —— 复制整条地址粘回来即可。
//
// 所以 NAS 上不需要监听任何回调端口，也不依赖浏览器能访问到 NAS。
// 这是这套流程能在群晖上跑通的关键。
package copilot

import (
	"errors"
	"fmt"
	"net/url"
	"strings"
	"sync"
	"time"

	"aggapi/internal/config"
	"aggapi/internal/provider/copilot/auth"
)

// loginTTL 是一次授权会话的有效期。
//
// 给 15 分钟：足够用户完成登录（含可能的密码/2FA），又不至于让
// 遗留的会话长期占着 state。过期后必须重新生成链接。
const loginTTL = 15 * time.Minute

// Login 管理「添加账号」的授权会话。
type Login struct {
	mu       sync.Mutex
	sessions map[string]*loginSession
}

type loginSession struct {
	verifier string
	created  time.Time
}

// NewLogin 构造授权管理器。
func NewLogin() *Login {
	return &Login{sessions: map[string]*loginSession{}}
}

// StartResult 是一次授权发起的结果，交给控制台展示。
type StartResult struct {
	// AuthURL 让用户在浏览器里打开。
	AuthURL string `json:"auth_url"`
	// State 是本次会话标识，提交回调地址时要带回来。
	State string `json:"state"`
	// RedirectURI 一并回显：用户需要知道「跳到哪个地址才算成功」，
	// 否则看到空白页会以为出错了。
	RedirectURI string `json:"redirect_uri"`
	// Hint 是给用户看的一句话说明。
	Hint string `json:"hint"`
	// ExpiresIn 是本次会话剩余有效期（秒）。
	ExpiresIn int `json:"expires_in"`
}

// Start 生成授权链接。
func (l *Login) Start() (StartResult, error) {
	verifier, err := auth.Verifier()
	if err != nil {
		return StartResult{}, fmt.Errorf("生成 PKCE 校验串失败：%w", err)
	}
	state, err := auth.Verifier() // 复用同一个随机源，长度足够
	if err != nil {
		return StartResult{}, fmt.Errorf("生成 state 失败：%w", err)
	}

	redirect := auth.RedirectURI()
	authURL := auth.AuthorizationURL(
		auth.AuthorizeEndpoint(), auth.ClientID(), redirect,
		state, auth.Challenge(verifier), auth.Scope())

	l.mu.Lock()
	l.gcLocked()
	l.sessions[state] = &loginSession{verifier: verifier, created: time.Now()}
	l.mu.Unlock()

	return StartResult{
		AuthURL:     authURL,
		State:       state,
		RedirectURI: redirect,
		Hint: "在浏览器里打开上面的链接并完成登录。" +
			"登录后浏览器会跳到一个空白页，把地址栏里的完整地址复制下来贴回这里。",
		ExpiresIn: int(loginTTL / time.Second),
	}, nil
}

// Finish 用用户粘贴的回调地址换取令牌，并组装成一个可入库的账号。
func (l *Login) Finish(state, pasted, displayName string) (config.Account, error) {
	l.mu.Lock()
	sess, ok := l.sessions[state]
	if ok {
		delete(l.sessions, state) // 一次性：无论成败都不允许复用
	}
	l.mu.Unlock()

	if !ok {
		return config.Account{}, errors.New("授权会话不存在或已过期，请重新生成授权链接")
	}
	if time.Since(sess.created) > loginTTL {
		return config.Account{}, errors.New("授权会话已超过 15 分钟，请重新生成授权链接")
	}

	code, err := extractCode(pasted)
	if err != nil {
		return config.Account{}, err
	}

	ts, err := auth.ExchangeCode(code, sess.verifier, auth.RedirectURI())
	if err != nil {
		return config.Account{}, fmt.Errorf("换取令牌失败：%w", err)
	}
	if ts.AccessToken == "" {
		return config.Account{}, errors.New("微软未返回 access_token")
	}

	name := strings.TrimSpace(displayName)
	if name == "" {
		name = firstNonEmpty(ts.DisplayName, ts.Email, "Copilot 账号")
	}

	return config.Account{
		Provider: "copilot",
		Name:     name,
		Auth: map[string]string{
			"access_token":  ts.AccessToken,
			"refresh_token": ts.RefreshToken,
			"oid":           ts.HomeOID,
			"tid":           ts.TenantID,
			"expires_at":    ts.ExpiresAt.Format(time.RFC3339),
			"email":         ts.Email,
		},
		Enabled:   true,
		CreatedAt: time.Now().UTC(),
	}, nil
}

// extractCode 从用户粘贴的内容里取出授权码。
//
// 三种输入都要能处理，因为用户实际会粘什么的都有：
//   - 完整的回调地址（最常见）
//   - 只有 code=... 的片段
//   - 光秃秃的一串码
func extractCode(pasted string) (string, error) {
	s := strings.TrimSpace(pasted)
	if s == "" {
		return "", errors.New("没有收到回调地址")
	}

	// 整条 URL
	if u, err := url.Parse(s); err == nil && u.Scheme != "" {
		if e := u.Query().Get("error"); e != "" {
			desc := u.Query().Get("error_description")
			return "", fmt.Errorf("微软返回了错误：%s %s", e, desc)
		}
		if c := u.Query().Get("code"); c != "" {
			return c, nil
		}
		return "", errors.New("地址里没有 code 参数，请确认复制的是登录后的完整地址")
	}

	// code=xxx 片段
	if i := strings.Index(s, "code="); i >= 0 {
		c := s[i+len("code="):]
		if j := strings.IndexAny(c, "&#"); j >= 0 {
			c = c[:j]
		}
		if c != "" {
			return c, nil
		}
	}

	// 光秃秃的一串码
	if !strings.ContainsAny(s, " \t\n") {
		return s, nil
	}
	return "", errors.New("无法从粘贴的内容里识别出授权码，请复制浏览器地址栏里的完整地址")
}

// gcLocked 清理过期会话。调用方必须已持锁。
func (l *Login) gcLocked() {
	now := time.Now()
	for k, v := range l.sessions {
		if now.Sub(v.created) > loginTTL {
			delete(l.sessions, k)
		}
	}
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return strings.TrimSpace(v)
		}
	}
	return ""
}
