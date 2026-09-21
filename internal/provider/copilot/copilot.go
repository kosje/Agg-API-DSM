// Package copilot 是 M365 Copilot 上游的实现。
//
// 迁移来源：M365-Copilot2API 的 internal/{auth,chathub,outbound}。
// 其中 auth 的「设备码授权」按用户要求**不迁移**（用不上）。
//
// 与 Agnes 上游的关键差异：Copilot 走 WebSocket 长连接，且连接有生命周期，
// 因此需要连接池（对应 M365 的 internal/chathub/connpool.go）。
// 这部分无法用标准库替代，是本项目保留 gorilla/websocket 的唯一原因。
//
// 当前状态：骨架。Models 已可对外暴露，Chat 尚未接通。
package copilot

import (
	"context"
	"fmt"

	"aggapi/internal/provider"
)

// Provider 实现 provider.Provider。
type Provider struct {
	dataDir string
}

func New(dataDir string) *Provider {
	return &Provider{dataDir: dataDir}
}

func (p *Provider) Name() string        { return "copilot" }
func (p *Provider) DisplayName() string { return "M365 Copilot" }

// Models 返回 Copilot 侧对外暴露的模型。
//
// 命名前缀 copilot- 与 Agnes 侧的 agnes- 刻意区分开：
// 模型名是路由的唯一依据，前缀让用户在客户端里一眼看出走的是哪条链路。
func (p *Provider) Models() []provider.Model {
	return []provider.Model{
		{
			ID:       "copilot-auto",
			Upstream: "auto",
			Caps:     provider.CapText | provider.CapImage | provider.CapVision | provider.CapDoc | provider.CapStream,
			Desc:     "Copilot 智能路由（含生图与文档解析）",
		},
		{
			ID:       "copilot-chat",
			Upstream: "chat",
			Caps:     provider.CapText | provider.CapVision | provider.CapDoc | provider.CapStream,
			Desc:     "纯文本对话",
		},
		{
			ID:       "copilot-image",
			Upstream: "image",
			Caps:     provider.CapImage,
			Desc:     "图片生成",
		},
	}
}

func (p *Provider) Health() provider.Health {
	return provider.Health{Ready: false, Detail: "尚未接入账号与连接池"}
}

func (p *Provider) Chat(ctx context.Context, req *provider.ChatRequest) (*provider.ChatResponse, error) {
	return nil, fmt.Errorf("%w：Copilot 上游尚未接通", provider.ErrUnsupported)
}

func (p *Provider) ChatStream(ctx context.Context, req *provider.ChatRequest, ch chan<- provider.StreamChunk) {
	defer close(ch)
	ch <- provider.StreamChunk{Err: fmt.Errorf("%w：Copilot 上游尚未接通", provider.ErrUnsupported)}
}
