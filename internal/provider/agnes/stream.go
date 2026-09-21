// Package agnes 的流式实现。
//
// Agnes 上游返回的是标准 SSE（`data: {...}` 行），所以这里只做一件事：
// 把 SSE 拆成行、取出增量文本，转成 provider.StreamChunk。
package agnes

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"aggapi/internal/config"
	"aggapi/internal/provider"
)

// ChatStream 执行一次流式对话。
func (p *Provider) ChatStream(ctx context.Context, req *provider.ChatRequest, ch chan<- provider.StreamChunk) {
	defer close(ch)

	acct, ok := req.Credential.(config.Account)
	if !ok {
		ch <- provider.StreamChunk{Err: fmt.Errorf("%w：Agnes 未收到账号凭据", provider.ErrNoCapacity)}
		return
	}

	body, err := p.buildBody(req)
	if err != nil {
		ch <- provider.StreamChunk{Err: err}
		return
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost,
		upstreamURL(acct, "/v1/chat/completions"), bytes.NewReader(body))
	if err != nil {
		ch <- provider.StreamChunk{Err: err}
		return
	}
	httpReq.Header.Set("Authorization", "Bearer "+acct.APIKey)
	httpReq.Header.Set("Content-Type", "application/json")
	// 显式声明接受 SSE：有些网关不带这个头就不会走流式。
	httpReq.Header.Set("Accept", "text/event-stream")
	httpReq.Header.Set("User-Agent", userAgent)

	resp, err := p.client.Do(httpReq)
	if err != nil {
		ch <- provider.StreamChunk{Err: fmt.Errorf("%w：%v", provider.ErrUpstream, err)}
		return
	}
	defer resp.Body.Close()

	// 错误要在**建立流之前**返回，这样网关还来得及改 HTTP 状态码。
	// 一旦开始往 ch 里塞正文，网关那边响应头就发出去了。
	if resp.StatusCode >= 400 {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		ch <- provider.StreamChunk{Err: classifyHTTP(acct, resp.StatusCode, string(raw))}
		return
	}

	sc := bufio.NewScanner(resp.Body)
	// 单行可能很长（生图返回的 base64 会塞在一条 data 里），
	// 默认 64KB 的缓冲不够用。
	sc.Buffer(make([]byte, 0, 64*1024), 4<<20)

	for sc.Scan() {
		select {
		case <-ctx.Done():
			ch <- provider.StreamChunk{Err: ctx.Err()}
			return
		default:
		}

		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, ":") {
			continue // 空行与注释（心跳）都跳过
		}
		if !strings.HasPrefix(line, "data:") {
			continue // event: / id: 之类，本上游用不到
		}
		data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if data == "[DONE]" {
			ch <- provider.StreamChunk{Done: true}
			return
		}

		var ev struct {
			Choices []struct {
				Delta struct {
					Content string `json:"content"`
				} `json:"delta"`
				// 有些上游把增量放在 message 里（首帧或非标准实现）
				Message struct {
					Content string `json:"content"`
				} `json:"message"`
				FinishReason *string `json:"finish_reason"`
			} `json:"choices"`
			Error *struct {
				Message string `json:"message"`
			} `json:"error"`
		}
		if err := json.Unmarshal([]byte(data), &ev); err != nil {
			// 单帧解析失败不该中断整条流 —— 上游偶尔会插入非标准帧。
			continue
		}
		if ev.Error != nil && ev.Error.Message != "" {
			ch <- provider.StreamChunk{Err: fmt.Errorf("%w：%s", provider.ErrUpstream, ev.Error.Message)}
			return
		}
		if len(ev.Choices) == 0 {
			continue
		}

		c := ev.Choices[0]
		text := c.Delta.Content
		if text == "" {
			text = c.Message.Content
		}
		if text != "" {
			ch <- provider.StreamChunk{Delta: text}
		}
		if c.FinishReason != nil && *c.FinishReason != "" {
			ch <- provider.StreamChunk{Done: true}
			return
		}
	}

	if err := sc.Err(); err != nil && ctx.Err() == nil {
		ch <- provider.StreamChunk{Err: fmt.Errorf("%w：读取流失败：%v", provider.ErrUpstream, err)}
		return
	}
	// 上游没发 [DONE] 就断流：补一个结束，免得网关那边一直等。
	ch <- provider.StreamChunk{Done: true}
}

// classifyHTTP 把上游的 HTTP 错误分类成网关认识的哨兵错误。
func classifyHTTP(acct config.Account, code int, body string) error {
	msg := truncate(strings.TrimSpace(body), 300)
	switch code {
	case http.StatusUnauthorized, http.StatusForbidden:
		return fmt.Errorf("%w：账号 %s 鉴权失败（HTTP %d）", provider.ErrAuth, acct.Name, code)
	case http.StatusTooManyRequests:
		return fmt.Errorf("%w：账号 %s 被限流", provider.ErrNoCapacity, acct.Name)
	default:
		return fmt.Errorf("%w：HTTP %d %s", provider.ErrUpstream, code, msg)
	}
}
