// Package outbound 提供出站 HTTP / WebSocket 客户端。
//
// 迁移来源：M365-Copilot2API 的 internal/outbound。
//
// **与上游的差异**：原版带一套代理池（SOCKS5 / HTTP 代理轮换），
// 那需要 golang.org/x/net/proxy。本项目的依赖策略是「只留标准库无法替代的」，
// 所以首版去掉了代理池，只保留直连与 SSRF 防护。
// 需要代理时再加回 —— 届时也只需改这一个包。
//
// # 关于 SSRF 防护的分层
//
// 直连 transport **故意不加**拨号期地址校验：它同时服务「运维自己配置的端点」
// （上游网关等），那些地址完全可能落在内网，加校验会误杀。
//
// 真正需要防护的是「从数据里读出来的 URL」——比如上游返回的图片链接。
// 这类请求走 UntrustedClientFrom，它在克隆出来的 transport 上加拨号期守卫。
// 这条边界很关键：**可信来源用直连客户端，不可信来源必须用 Untrusted 版本。**
package outbound

import (
	"context"
	"crypto/tls"
	"fmt"
	"net"
	"net/http"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

// Clients 是一组配套的客户端（HTTP 与 WebSocket 共用同一份 TLS 会话缓存）。
type Clients struct {
	HTTP      *http.Client
	WebSocket *websocket.Dialer
}

var (
	once    sync.Once
	direct  *Clients
	tlsOnce sync.Once
)

// tlsCache 让 HTTP 与 WebSocket 共用会话票据缓存。
//
// 两者访问的是同一批主机，共用缓存能省掉大量重复的 TLS 握手 ——
// Copilot 的 WebSocket 建连很频繁，这个开销不能忽略。
var tlsCache tls.ClientSessionCache

func initTLS() {
	tlsOnce.Do(func() {
		tlsCache = tls.NewLRUClientSessionCache(32)
	})
}

func directClients() *Clients {
	initTLS()
	httpTLSConf := &tls.Config{ClientSessionCache: tlsCache}
	// WebSocket 强制 HTTP/1.1：RFC 6455 的升级握手不支持 HTTP/2。
	wsTLSConf := &tls.Config{
		ClientSessionCache: tlsCache,
		NextProtos:         []string{"http/1.1"},
	}

	t := &http.Transport{
		Proxy:                 nil,
		DialContext:           (&net.Dialer{Timeout: 30 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
		MaxIdleConns:          100,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
		ForceAttemptHTTP2:     true,
		TLSClientConfig:       httpTLSConf,
	}
	return &Clients{
		HTTP: &http.Client{Transport: t},
		WebSocket: &websocket.Dialer{
			HandshakeTimeout: 20 * time.Second,
			// 读缓冲给大一些：Copilot 的流式事件单帧可能很长。
			ReadBufferSize:  256 * 1024,
			WriteBufferSize: 16 * 1024,
			NetDialContext:  t.DialContext,
			TLSClientConfig: wsTLSConf,
		},
	}
}

// HTTPClient 返回直连 HTTP 客户端（无可信度要求时使用）。
func HTTPClient() *http.Client {
	once.Do(func() { direct = directClients() })
	return direct.HTTP
}

// WebSocketDialer 返回直连 WebSocket 拨号器。
//
// 返回副本：调用方可能改字段（例如加自定义头），
// 共享同一个 Dialer 会让改动互相污染。
func WebSocketDialer() *websocket.Dialer {
	once.Do(func() { direct = directClients() })
	d := *direct.WebSocket
	return &d
}

// UntrustedClientFrom 克隆一个客户端并加上拨号期 SSRF 守卫。
//
// base 为 nil 时用直连客户端。用于抓取「从数据里读出来的 URL」。
func UntrustedClientFrom(base *http.Client, timeout time.Duration) *http.Client {
	if base == nil {
		base = HTTPClient()
	}
	tr, ok := base.Transport.(*http.Transport)
	if !ok || tr == nil {
		tr = &http.Transport{}
	}
	cloned := tr.Clone()
	// 只改拨号这一层：其余（TLS 缓存、连接池参数）保持与 base 一致。
	dialer := &net.Dialer{Timeout: 15 * time.Second, KeepAlive: 30 * time.Second}
	cloned.DialContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
		host, _, err := net.SplitHostPort(addr)
		if err != nil {
			return nil, err
		}
		ips, err := net.DefaultResolver.LookupIPAddr(ctx, host)
		if err != nil {
			return nil, err
		}
		for _, ip := range ips {
			if IsUnsafeIP(ip.IP) {
				return nil, fmt.Errorf("拒绝连接内网地址 %s（%s）", ip.IP, host)
			}
		}
		return dialer.DialContext(ctx, network, addr)
	}
	c := *base
	c.Transport = cloned
	if timeout > 0 {
		c.Timeout = timeout
	}
	return &c
}
