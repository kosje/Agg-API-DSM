// Package copilot 的流式实现。
//
// Copilot 走 WebSocket，产出方式与 Agnes 的 SSE 完全不同 ——
// 但 chathub 已经把「增量回调」封装好了（ChatWithDelta），
// 这里只负责把它接到 provider.StreamChunk 上。
package copilot

import (
	"context"
	"fmt"

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
