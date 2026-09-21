// Package agnes 是 Agnes AI 上游的实现。
//
// 迁移来源：agnes-hub-go 的 internal/{pool,hub,relay,intent}。
// 那里把「账号池 + 限流 + 意图判定」与「Agnes 协议适配」揉在一起；
// 新架构把前者上提到网关层共用，本包只留协议适配部分。
//
// 当前状态：骨架。Models 已可对外暴露，Chat 尚未接通。
package agnes

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

func (p *Provider) Name() string        { return "agnes" }
func (p *Provider) DisplayName() string { return "Agnes AI" }

// Models 返回 Agnes 侧对外暴露的模型。
//
// 迁移要点：agnes-hub-go 的 model_manifest 支持账号级声明，模型清单是动态的；
// 这里先给静态清单，待账号池接入后改为按已配置账号聚合。
func (p *Provider) Models() []provider.Model {
	return []provider.Model{
		{
			ID:       "agnes-auto",
			Upstream: "agnes-auto",
			Caps:     provider.CapText | provider.CapImage | provider.CapVideo | provider.CapStream,
			Desc:     "自动判定文生 / 生图 / 生视频",
		},
	}
}

func (p *Provider) Health() provider.Health {
	return provider.Health{Ready: false, Detail: "尚未接入账号池"}
}

func (p *Provider) Chat(ctx context.Context, req *provider.ChatRequest) (*provider.ChatResponse, error) {
	return nil, fmt.Errorf("%w：Agnes 上游尚未接通", provider.ErrUnsupported)
}

func (p *Provider) ChatStream(ctx context.Context, req *provider.ChatRequest, ch chan<- provider.StreamChunk) {
	defer close(ch)
	ch <- provider.StreamChunk{Err: fmt.Errorf("%w：Agnes 上游尚未接通", provider.ErrUnsupported)}
}
