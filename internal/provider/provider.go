// Package provider 定义「上游」的统一抽象。
//
// 这是整个网关的枢纽：两个来源完全不同的上游（Agnes AI 与 M365 Copilot）
// 必须收敛成同一个形状，网关核心才能不认识它们、只做路由与转发。
//
// 设计要点：
//
//  1. **网关核心不认识任何具体上游。** 它只持有 []Provider，按模型名路由。
//     新增第三个上游时，只需实现本接口并在装配处注册，核心代码零改动。
//
//  2. **账号池与限流是「共用能力」，不是某个上游的私产。**
//     Agnes 那边的多账号池化 + RPM 严格节拍限流，对 Copilot 同样适用
//     （Copilot 也有配额与冷却）。所以 Pool/Pacer 放在 provider 之外，
//     由两者共享 —— 这正是「互补而非重叠」的落点。
//
//  3. **能力用 Capability 声明，而不是让调用方猜。**
//     某个上游不支持图片时，网关据此直接返回明确错误，而不是转发出去
//     拿到一个语焉不详的上游报错。
package provider

import (
	"context"
	"errors"
)

// Capability 描述一个上游具备的能力位。
// 用位掩码而非多个 bool，是为了让「模型是否可用于某请求」的判断保持一行。
type Capability uint32

const (
	// CapText 文本对话
	CapText Capability = 1 << iota
	// CapImage 图片生成
	CapImage
	// CapVision 图片理解（多模态输入）
	CapVision
	// CapVideo 视频生成
	CapVideo
	// CapDoc 文档附件解析（PDF / Excel 等）
	CapDoc
	// CapStream 流式响应
	CapStream
)

// Has 判断是否同时具备给定能力。
func (c Capability) Has(want Capability) bool { return c&want == want }

func (c Capability) String() string {
	names := []struct {
		bit  Capability
		name string
	}{
		{CapText, "text"}, {CapImage, "image"}, {CapVision, "vision"},
		{CapVideo, "video"}, {CapDoc, "doc"}, {CapStream, "stream"},
	}
	out := ""
	for _, n := range names {
		if c&n.bit != 0 {
			if out != "" {
				out += ","
			}
			out += n.name
		}
	}
	return out
}

// Model 是一个上游暴露的模型。网关把它汇总成统一的 /v1/models 列表。
type Model struct {
	// ID 是对外暴露的模型名。必须全局唯一 —— 路由就靠它。
	ID string
	// Upstream 是该模型在上游侧的真实标识（可能与 ID 不同）。
	Upstream string
	// Caps 是这个模型支持的能力。
	Caps Capability
	// Desc 给控制台显示用。
	Desc string
}

// ChatRequest 是网关归一化之后的对话请求。
//
// 刻意不使用任何 OpenAI 的 struct：两个上游的协议差异很大，
// 归一化成自己的形状再各自适配，比让两边都去迁就某一方的结构要稳。
type ChatRequest struct {
	Model    string
	Messages []Message
	// Stream 由网关决定是否真正流式，上游不支持时会退化为一次性返回。
	Stream      bool
	Temperature float64
	MaxTokens   int
	// Images 是随消息附带的图片（URL 或 base64），由支持 CapVision 的上游消费。
	Images []string
	// Docs 是附件（文件名 + 原始字节），由支持 CapDoc 的上游消费。
	Docs []Document
	// Raw 保留调用方原始请求体，供上游需要透传字段时使用。
	Raw map[string]any
}

// Message 是一条归一化消息。
type Message struct {
	Role    string // system / user / assistant
	Content string
}

// Document 是一个附件。
type Document struct {
	Name string
	MIME string
	Data []byte
}

// ChatResponse 是归一化之后的回复。
type ChatResponse struct {
	Model   string
	Content string
	// Images 是上游返回的图片（URL 或 base64）。
	Images []string
	Usage  Usage
	// Raw 保留上游原始响应，供调试与透传。
	Raw map[string]any
}

// Usage 是 token 用量。两个上游统计口径不同，统一到这里。
type Usage struct {
	PromptTokens     int
	CompletionTokens int
	TotalTokens      int
}

// StreamChunk 是流式响应的一段。
type StreamChunk struct {
	Delta string
	Done  bool
	Err   error
}

// Health 描述上游当前的健康状况，供控制台展示。
type Health struct {
	Ready bool
	// Detail 是给人看的一句话，例如「3 个账号可用，2 个熔断中」。
	Detail string
}

// Provider 是所有上游必须实现的接口。
//
// 刻意保持窄：只有「我是谁」「我有哪些模型」「我能不能干活」「干活」四件事。
// 账号池、限流、重试、熔断都在网关层统一处理 —— 上游实现者不必重复造。
type Provider interface {
	// Name 是上游的稳定标识，用于配置、日志与路由前缀，例如 "agnes" / "copilot"。
	Name() string
	// DisplayName 给控制台显示，例如 "Agnes AI" / "M365 Copilot"。
	DisplayName() string
	// Models 返回当前可用的模型列表。配置变化时应能动态反映。
	Models() []Model
	// Health 返回健康状况。
	Health() Health
	// Chat 执行一次对话。ctx 取消时必须尽快返回。
	Chat(ctx context.Context, req *ChatRequest) (*ChatResponse, error)
	// ChatStream 执行一次流式对话。实现方把分片写入 ch 并在结束时关闭它。
	// 不支持流式的上游可以只发一个 Done 分片。
	ChatStream(ctx context.Context, req *ChatRequest, ch chan<- StreamChunk)
}

// 网关层的通用错误。上游实现应当用这些包装，便于网关统一映射成 HTTP 状态码。
var (
	// ErrNoCapacity 表示上游暂时没有可用账号（全部熔断 / 未配置）。
	ErrNoCapacity = errors.New("no capacity")
	// ErrUnsupported 表示上游不支持该能力。
	ErrUnsupported = errors.New("unsupported capability")
	// ErrUpstream 表示上游返回了错误。
	ErrUpstream = errors.New("upstream error")
	// ErrAuth 表示上游鉴权失败（凭据过期等），需要用户重新授权。
	ErrAuth = errors.New("upstream auth required")
)
