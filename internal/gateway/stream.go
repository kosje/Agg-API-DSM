// Package gateway 的流式输出。
//
// 下游要的是 OpenAI 的 SSE 格式（`data: {...}\n\n`，最后以 `data: [DONE]` 收尾）。
// 两个上游的产出方式完全不同 —— Agnes 是标准 SSE，Copilot 是 WebSocket 增量回调 ——
// 但它们都被归一化成 provider.StreamChunk，所以这一层只认 StreamChunk。
package gateway

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"aggapi/internal/provider"
)

// sseHeartbeat 是心跳间隔。
//
// 上游可能长时间不吐字（尤其推理模型「想」的阶段）。没有心跳的话，
// 中间的反向代理（nginx / 群晖反代 / Cloudflare）会认为连接空闲而掐断，
// 用户看到的就是「说到一半停了」。
const sseHeartbeat = 15 * time.Second

// writeSSEHeaders 写好 SSE 响应头。
//
// 必须显式 Flush：不刷的话响应头会卡在 Go 的缓冲区里，
// 客户端以为服务端还没开始响应。
func writeSSEHeaders(w http.ResponseWriter) (http.Flusher, error) {
	w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	// 关掉 nginx 一类反代的缓冲，否则流式会被攒成一坨再发。
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	f, ok := w.(http.Flusher)
	if !ok {
		return nil, fmt.Errorf("响应不支持流式（ResponseWriter 未实现 Flusher）")
	}
	f.Flush()
	return f, nil
}

// sseWrite 写一条 SSE 事件并立即刷出。
func sseWrite(w http.ResponseWriter, f http.Flusher, payload string) error {
	if _, err := fmt.Fprintf(w, "data: %s\n\n", payload); err != nil {
		return err
	}
	f.Flush()
	return nil
}

// chunkToOpenAI 把归一化分片转成 OpenAI 的 chunk 结构。
//
// 即使内容为空也要发（带 role 的那一帧）—— 客户端靠它初始化消息气泡。
func chunkToOpenAI(model, id string, first bool, delta provider.StreamChunk) map[string]any {
	d := map[string]any{}
	if first {
		d["role"] = "assistant"
	}
	if delta.Delta != "" {
		d["content"] = delta.Delta
	}
	finish := any(nil)
	if delta.Done {
		finish = "stop"
	}
	return map[string]any{
		"id":     id,
		"object": "chat.completion.chunk",
		"model":  model,
		"choices": []map[string]any{{
			"index":         0,
			"delta":         d,
			"finish_reason": finish,
		}},
	}
}

// streamChat 把上游的流式产出转成下游的 SSE。
//
// 出错处理是这里最需要斟酌的地方：一旦响应头发出去，就**无法再改 HTTP 状态码**，
// 只能把错误写进流里让客户端看到。所以：
//   - 拿不到第一个分片之前就失败 → 还能正常返回 JSON 错误
//   - 已经开始流式之后失败 → 发一条 error 事件，再补 [DONE] 收尾，
//     否则客户端会一直等一个永远不来的结束标记
func (h *Handler) streamChat(w http.ResponseWriter, r *http.Request,
	up provider.Provider, model, publicModel string, req *provider.ChatRequest,
	sessionKey string) {

	ch := make(chan provider.StreamChunk, 8)
	ctx := r.Context()

	// 领账号 + 取第一个分片，失败就换账号重试。
	//
	// 为什么要在这里重试：流式的错误大多在第一个分片就暴露（鉴权、限流），
	// 那时响应头还没发出去，换账号是可行的。一旦开始往客户端写正文，
	// 就再也改不了响应，只能把错误塞进流里。
	var (
		lease   provider.Lease
		first   provider.StreamChunk
		tried   []string
		lastErr error
		ok      bool
	)
	for attempt := 0; attempt <= h.maxRetries; attempt++ {
		lease, lastErr = up.Acquire(ctx, sessionKey, tried...)
		if lastErr != nil {
			break
		}
		req.Credential = lease.Credential()

		// 上游产出与下游写入解耦：上游写 channel，这里读。
		// 缓冲 8 是折中 —— 太小会让上游阻塞在写上，太大则首字延迟变高。
		ch = make(chan provider.StreamChunk, 8)
		go up.ChatStream(ctx, req, ch)

		select {
		case first, ok = <-ch:
			if !ok {
				lastErr = fmt.Errorf("上游没有返回任何内容")
			} else {
				lastErr = first.Err
			}
		case <-ctx.Done():
			lease.Release(ctx.Err())
			return
		}
		// 账号一直用到流结束才归还：提前归还的话，池子会以为它空闲了，
		// 可能同时把这个账号再发给别的请求。
		lease.Release(lastErr)
		if lastErr == nil {
			break
		}
		tried = append(tried, lease.AccountID())
		if !retriable(lastErr) {
			break
		}
		if attempt < h.maxRetries {
			time.Sleep(backoff(attempt))
		}
	}
	if lastErr != nil {
		writeAPIError(w, errorBody(lastErr))
		return
	}

	f, err := writeSSEHeaders(w)
	if err != nil {
		return
	}

	id := "chatcmpl-" + shortID()

	send := func(c provider.StreamChunk, isFirst bool) bool {
		b, err := json.Marshal(chunkToOpenAI(publicModel, id, isFirst, c))
		if err != nil {
			return false
		}
		return sseWrite(w, f, string(b)) == nil
	}

	if !send(first, true) {
		return
	}
	if first.Done {
		_ = sseWrite(w, f, "[DONE]")
		return
	}

	// 心跳：上游长时间不吐字时发注释行（以 ":" 开头），
	// SSE 规范里客户端会忽略它，但足以让中间设备认为连接还活着。
	hb := time.NewTicker(sseHeartbeat)
	defer hb.Stop()

	for {
		select {
		case <-ctx.Done():
			// 客户端断开：不再往下写，让上游的 goroutine 随 ctx 一起结束。
			return
		case <-hb.C:
			if _, err := fmt.Fprint(w, ": ping\n\n"); err != nil {
				return
			}
			f.Flush()
		case c, ok := <-ch:
			if !ok {
				// 上游正常收尾但没发 Done：补上结束标记。
				_ = sseWrite(w, f, "[DONE]")
				return
			}
			if c.Err != nil {
				// 已经开始流式，改不了状态码了 —— 把错误写进流，
				// 再补 [DONE]，否则客户端会一直等结束标记。
				payload, _ := json.Marshal(map[string]any{
					"error": map[string]any{
						"message": c.Err.Error(),
						"type":    "upstream_error",
					},
				})
				_ = sseWrite(w, f, string(payload))
				_ = sseWrite(w, f, "[DONE]")
				return
			}
			if !send(c, false) {
				return
			}
			if c.Done {
				_ = sseWrite(w, f, "[DONE]")
				return
			}
		}
	}
}

// shortID 给流式响应一个稳定的 id 前缀。
//
// 用时间戳而不是随机串：客户端日志里看到 id 就能大致判断时间，
// 排查「这次请求是什么时候发的」时很有用。
func shortID() string {
	return strings.ReplaceAll(time.Now().UTC().Format("20060102T150405.000000"), ".", "")
}
