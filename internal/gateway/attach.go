// Package gateway 的附件处理。
//
// OpenAI 的 /v1/chat/completions 规范里没有「文件附件」这一说，各家客户端
// 各显神通。这里支持两种最常见的形态：
//
//  1. `image_url` 里的 data URL（`data:image/png;base64,...`）→ 交给支持
//     视觉的上游，作为图片输入
//  2. 顶层 `attachments: [{name, mime, data}]`（data 为 base64）→ 本地解析成
//     文本，拼进用户消息
//
// 刻意**不猜**别的字段名：猜错会把一段二进制当文本塞给模型，
// 模型输出的东西会莫名其妙，比直接说「不支持」更难查。
package gateway

import (
	"encoding/base64"
	"fmt"
	"strings"

	"aggapi/internal/docs"
	"aggapi/internal/provider"
)

// parseAttachments 从原始请求里抽出附件。
//
// 返回的文本会**拼在用户消息前面**而不是替换它：用户常常是「这份文件讲了什么？」
// 这种带指代的问题，把文件内容顶掉提问会让模型无从回答。
func parseAttachments(raw map[string]any) (images []string, docText string, err error) {
	// 1) 顶层 attachments
	if v, ok := raw["attachments"]; ok {
		arr, ok := v.([]any)
		if !ok {
			return nil, "", fmt.Errorf("attachments 必须是数组")
		}
		var parts []string
		for i, item := range arr {
			m, ok := item.(map[string]any)
			if !ok {
				continue
			}
			name, _ := m["name"].(string)
			mime, _ := m["mime"].(string)
			b64, _ := m["data"].(string)
			if b64 == "" {
				continue
			}
			if !docs.Supported(name) {
				return nil, "", fmt.Errorf(
					"附件 %q 的类型不支持（支持 PDF / XLSX / CSV / TSV / 纯文本）", name)
			}
			data, derr := base64.StdEncoding.DecodeString(b64)
			if derr != nil {
				// 客户端可能用 URL-safe 编码
				data, derr = base64.RawURLEncoding.DecodeString(b64)
				if derr != nil {
					return nil, "", fmt.Errorf("附件 %q 的 base64 无法解码", name)
				}
			}
			if len(data) > docs.MaxBytes {
				return nil, "", fmt.Errorf("附件 %q 超过 %d MB 上限", name, docs.MaxBytes>>20)
			}
			text, xerr := docs.Extract(name, mime, data)
			if xerr != nil {
				return nil, "", fmt.Errorf("解析附件 %q 失败：%w", name, xerr)
			}
			if strings.TrimSpace(text) == "" {
				// 扫描件 PDF 没有文字层。硬塞空内容会让模型以为文件是空的，
				// 不如明确告诉用户。
				return nil, "", fmt.Errorf(
					"附件 %q 里没有可提取的文字（扫描件需要 OCR，本网关不做）", name)
			}
			parts = append(parts, fmt.Sprintf("【附件 %d：%s】\n%s", i+1, name, text))
		}
		if len(parts) > 0 {
			docText = strings.Join(parts, "\n\n")
		}
	}

	// 2) 消息里的 image_url data URL
	images = collectDataURLs(raw)
	return images, docText, nil
}

// collectDataURLs 从 messages 的 content 数组里收集图片 data URL。
//
// 只收 data URL，不收 http(s) 链接：后者会让上游去访问一个用户给的地址，
// 那是 SSRF 面。真要支持远程图片，应当由网关自己下载并做地址校验。
func collectDataURLs(raw map[string]any) []string {
	msgs, ok := raw["messages"].([]any)
	if !ok {
		return nil
	}
	var out []string
	for _, m := range msgs {
		msg, ok := m.(map[string]any)
		if !ok {
			continue
		}
		parts, ok := msg["content"].([]any)
		if !ok {
			continue
		}
		for _, p := range parts {
			part, ok := p.(map[string]any)
			if !ok {
				continue
			}
			if u, ok := part["image_url"].(string); ok && strings.HasPrefix(u, "data:") {
				out = append(out, u)
				continue
			}
			// 有些客户端把 image_url 写成对象 {url: "..."}
			if obj, ok := part["image_url"].(map[string]any); ok {
				if u, ok := obj["url"].(string); ok && strings.HasPrefix(u, "data:") {
					out = append(out, u)
				}
			}
		}
	}
	return out
}

// attachDocs 把解析出的文档文本拼进最后一条用户消息。
func attachDocs(req *provider.ChatRequest, docText string) {
	if docText == "" {
		return
	}
	// 从后往前找第一条 user 消息：多轮对话里最后一条才是本次提问。
	for i := len(req.Messages) - 1; i >= 0; i-- {
		if req.Messages[i].Role == "user" {
			req.Messages[i].Content = docText + "\n\n" + req.Messages[i].Content
			return
		}
	}
	// 没有 user 消息（少见）：单独造一条。
	req.Messages = append(req.Messages, provider.Message{
		Role: "user", Content: docText,
	})
}
