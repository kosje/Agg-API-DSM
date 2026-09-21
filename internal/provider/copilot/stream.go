// Package copilot 的流式实现。
//
// Copilot 走 WebSocket，产出方式与 Agnes 的 SSE 完全不同 ——
// 但 chathub 已经把「增量回调」封装好了（ChatWithDelta），
// 这里只负责把它接到 provider.StreamChunk 上。
package copilot

import (
	"context"
	"fmt"
	"io"
	"net/http"

	"aggapi/internal/config"
	"aggapi/internal/provider"
)

// ChatStream 执行一次流式对话。
func (p *Provider) ChatStream(ctx context.Context, req *provider.ChatRequest, ch chan<- provider.StreamChunk) {
	defer close(ch)

	acct, ok := req.Credential.(config.Account)
	if !ok {
		ch <- provider.StreamChunk{Err: fmt.Errorf("%w：Copilot 未收到账号凭据", provider.ErrNoCapacity)}
		return
	}
	acct = p.ensureFresh(ctx, acct)
	ca, err := toChatHubAccount(acct)
	if err != nil {
		ch <- provider.StreamChunk{Err: err}
		return
	}

	creq := buildRequest(req)

	_, err = p.client.ChatWithDelta(ctx, ca, creq, func(delta string) error {
		if delta == "" {
			return nil
		}
		select {
		case ch <- provider.StreamChunk{Delta: delta}:
			return nil
		case <-ctx.Done():
			// 下游断开：返回错误让 chathub 尽快收手，别继续读上游。
			return ctx.Err()
		}
	})
	if err != nil {
		ch <- provider.StreamChunk{Err: classifyChatErr(err)}
		return
	}
	ch <- provider.StreamChunk{Done: true}
}

// FetchImage 带令牌下载上游生成的图片。
//
// 上游把图放在需要鉴权的临时地址上，不带 Authorization 会拿到 401。
// 令牌取自本次占用的账号 —— 生成这张图的正是它。
func (p *Provider) FetchImage(ctx context.Context, credential any, rawURL string) ([]byte, string, error) {
	acct, ok := credential.(config.Account)
	if !ok {
		return nil, "", fmt.Errorf("%w：Copilot 未收到账号凭据", provider.ErrNoCapacity)
	}
	acct = p.ensureFresh(ctx, acct)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, "", err
	}
	req.Header.Set("Authorization", "Bearer "+acct.Auth["access_token"])
	req.Header.Set("Accept", "image/*")
	req.Header.Set("User-Agent", "Agg-API-DSM/1.0")

	resp, err := p.imageClient.Do(req)
	if err != nil {
		return nil, "", fmt.Errorf("%w：下载图片失败：%v", provider.ErrUpstream, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return nil, "", fmt.Errorf("%w：下载图片被拒（HTTP %d）", provider.ErrUpstream, resp.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, 24<<20))
	if err != nil {
		return nil, "", fmt.Errorf("%w：读取图片失败：%v", provider.ErrUpstream, err)
	}
	mime := resp.Header.Get("Content-Type")
	if mime == "" {
		mime = http.DetectContentType(data)
	}
	return data, mime, nil
}
