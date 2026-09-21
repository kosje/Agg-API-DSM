// Package copilot 的模型目录。
//
// 上游用 **tone** 而不是模型名来区分模型与推理深度。
// 但对外必须暴露**用户认得的名字** —— 在客户端里看到 `copilot-chat`
// 这种自造名，没人知道它对应哪个模型，也没法按需切换。
//
// 所以这里维护一张「对外模型名 -> 上游 tone」的映射表，取值对齐
// M365-Copilot2API 的 gatewayModels：那是实测可用的一组。
package copilot

import "aggapi/internal/provider"

// modelSpec 是目录里的一项。
type modelSpec struct {
	// ID 是对外暴露的模型名，直接用微软那边的叫法。
	ID string
	// Tone 是上游认的标识。空表示走智能路由（magic）。
	Tone string
	// Caps 能力位。不写默认按文本处理。
	Caps provider.Capability
	// Desc 给控制台显示。
	Desc string
}

// catalog 是 Copilot 侧对外暴露的模型。
//
// 顺序即控制台与 /v1/models 里的展示顺序：把常用的放前面。
var catalog = []modelSpec{
	{ID: "copilot-auto", Tone: "magic", Desc: "智能路由（上游自己挑模型）",
		Caps: provider.CapText | provider.CapImage | provider.CapVision | provider.CapDoc | provider.CapStream},

	{ID: "gpt-5.2", Tone: "Gpt_5_2_Chat", Desc: "GPT-5.2"},
	{ID: "gpt-5.2-reasoning", Tone: "Gpt_5_2_Reasoning", Desc: "GPT-5.2 推理"},

	{ID: "gpt-5.3", Tone: "Gpt_5_3_Chat", Desc: "GPT-5.3"},
	{ID: "gpt-5.3-reasoning", Tone: "Gpt_5_3_Reasoning", Desc: "GPT-5.3 推理"},
	{ID: "gpt-5.3-think-deeper", Tone: "Gpt_5_3_Chat", Desc: "GPT-5.3 深度思考"},

	{ID: "gpt-5.4", Tone: "Gpt_5_4_Chat", Desc: "GPT-5.4"},
	{ID: "gpt-5.4-reasoning", Tone: "Gpt_5_4_Reasoning", Desc: "GPT-5.4 推理"},
	{ID: "gpt-5.4-quick", Tone: "Gpt_5_4_Chat", Desc: "GPT-5.4 快速"},
	{ID: "gpt-5.4-mini", Tone: "Gpt_5_4_Chat", Desc: "GPT-5.4 Mini"},

	{ID: "gpt-5.5", Tone: "Gpt_5_5_Chat", Desc: "GPT-5.5"},
	{ID: "gpt-5.5-reasoning", Tone: "Gpt_5_5_Reasoning", Desc: "GPT-5.5 推理"},

	{ID: "gpt-5.6-reasoning", Tone: "Gpt_5_6_Reasoning", Desc: "GPT-5.6 推理"},
	{ID: "gpt-5.6-sol", Tone: "Gpt_5_6_Reasoning", Desc: "GPT-5.6 Sol"},
	{ID: "gpt-5.6-terra", Tone: "Gpt_5_6_Reasoning", Desc: "GPT-5.6 Terra"},
	{ID: "gpt-5.6-luna", Tone: "Gpt_5_6_Reasoning", Desc: "GPT-5.6 Luna"},

	{ID: "claude-sonnet", Tone: "Claude_Sonnet", Desc: "Claude Sonnet"},
	{ID: "claude-sonnet-reasoning", Tone: "Claude_Sonnet_Reasoning", Desc: "Claude Sonnet 推理"},
	{ID: "claude", Tone: "Claude_Sonnet", Desc: "Claude Sonnet（别名）"},

	{ID: "codex-auto-review", Tone: "magic", Desc: "代码审查"},

	{ID: "gpt-image-2", Tone: "magic", Desc: "图片生成",
		Caps: provider.CapImage},
}

// defaultCaps 是未显式声明能力时的默认值。
//
// 默认给「文本 + 流式」而不是全给：判成支持而实际不支持，
// 请求会一路走到上游才失败；判成不支持只是少一个入口。
const defaultCaps = provider.CapText | provider.CapStream

// models 把目录转成 provider.Model 列表。
func models() []provider.Model {
	out := make([]provider.Model, 0, len(catalog))
	for _, s := range catalog {
		caps := s.Caps
		if caps == 0 {
			caps = defaultCaps
		}
		out = append(out, provider.Model{
			ID:       s.ID,
			Upstream: s.Tone,
			Caps:     caps,
			Desc:     s.Desc,
		})
	}
	return out
}

// specFor 按对外模型名查目录。
func specFor(id string) (modelSpec, bool) {
	for _, s := range catalog {
		if s.ID == id {
			return s, true
		}
	}
	return modelSpec{}, false
}
