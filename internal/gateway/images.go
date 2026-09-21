// Package gateway 的生图端点。
//
// OpenAI 规范里生图走 `/v1/images/generations`，与对话是两条路径。
// 但 Copilot 上游**没有单独的生图接口** —— 它是把生图意图当成普通对话
// 发给模型的（tone=magic），模型自己决定要不要画。返回的消息里带图片 URL。
//
// 所以这里做的其实是「把对话结果翻译成 OpenAI 生图响应」：
//  1. 用提示词发一次对话
//  2. 从返回里取出图片 URL
//  3. 按客户端要的格式返回（url 或 b64_json）
//
// 关键差异点：**b64_json 需要把图片下载下来再编码**。上游给的是
// 需要鉴权的临时 URL，直接把 URL 透给客户端它自己下不了；
// 而 url 模式返回的是网关自己的下载地址，由网关代下。
package gateway

import (
	"context"
	"encoding/base64"
	"fmt"
	"net/http"
	"strings"
	"time"

	"aggapi/internal/provider"
)

// imageRequest 是 /v1/images/generations 的请求体。
type imageRequest struct {
	Prompt string `json:"prompt"`
	Model  string `json:"model"`
	// N 是请求张数。Copilot 上游一次只出一张，多张靠并发多次请求，
	// 这里先只支持 1，传更大的值明确报错而不是静默少给。
	N int `json:"n"`
	// Size / Quality / Style 上游不支持，收了但不生效 —— 明确忽略
	// 比报错好：多数客户端会带默认值，为此拒绝请求太苛刻。
	Size    string `json:"size"`
	Quality string `json:"quality"`
	Style   string `json:"style"`
	// ResponseFormat 是 url 或 b64_json。
	ResponseFormat string `json:"response_format"`
	// User 忽略（OpenAI 用它做滥用追踪）。
	User string `json:"user"`
}

// handleImages 处理 /v1/images/generations。
func (h *Handler) handleImages(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeAPIError(w, &APIError{
			Code: http.StatusMethodNotAllowed, Message: "只支持 POST", Type: "invalid_request_error",
		})
		return
	}
	if !h.authorize(w, r) {
		return
	}

	body, err := readJSON[imageRequest](r)
	if err != nil {
		writeAPIError(w, &APIError{
			Code: http.StatusBadRequest, Message: err.Error(), Type: "invalid_request_error",
		})
		return
	}
	if strings.TrimSpace(body.Prompt) == "" {
		writeAPIError(w, &APIError{
			Code: http.StatusBadRequest, Message: "prompt 不能为空",
			Type: "invalid_request_error", Param: "prompt",
		})
		return
	}
	if body.N > 1 {
		writeAPIError(w, &APIError{
			Code: http.StatusBadRequest,
			Message: "上游一次只生成一张图，n 只能为 1",
			Type:  "invalid_request_error", Param: "n",
		})
		return
	}
	// 默认返回 b64_json（图片本体）。
	//
	// OpenAI 的默认是 url，但上游的 URL 需要令牌鉴权，客户端取不到；
	// 而网关地址又要求客户端能访问到对外地址。返回本体最稳 ——
	// 一次往返拿到全部数据，不依赖任何后续可达性。
	format := body.ResponseFormat
	if format == "" {
		format = "b64_json"
	}
	if format != "url" && format != "b64_json" {
		writeAPIError(w, &APIError{
			Code: http.StatusBadRequest, Message: "response_format 只能是 url 或 b64_json",
			Type: "invalid_request_error", Param: "response_format",
		})
		return
	}

	// 挑一个声明了生图能力的模型。
	model, up, err := h.router.PickImageModel(body.Model)
	if err != nil {
		writeAPIError(w, errorBody(err))
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), h.imageTimeout)
	defer cancel()

	preq := &provider.ChatRequest{
		Model: model.Upstream,
		Messages: []provider.Message{
			{Role: "user", Content: body.Prompt},
		},
	}

	var (
		resp    *provider.ChatResponse
		lastErr error
		tried   []string
	)
	for attempt := 0; attempt <= h.maxRetries; attempt++ {
		lease, aerr := up.Acquire(ctx, "", tried...)
		if aerr != nil {
			if lastErr != nil {
				break
			}
			writeAPIError(w, errorBody(aerr))
			return
		}
		preq.Credential = lease.Credential()
		resp, lastErr = up.Chat(ctx, preq)
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

	if len(resp.Images) == 0 {
		// 上游没出图。把它的文字回复带回去 —— 通常是「这个请求我不能画」
		// 之类的拒绝原因，比干巴巴一句「没有图片」有用得多。
		msg := strings.TrimSpace(resp.Content)
		if msg == "" {
			msg = "上游没有返回图片"
		}
		writeAPIError(w, &APIError{
			Code: http.StatusBadGateway, Message: msg, Type: "upstream_error",
		})
		return
	}

	// 上游的图片 URL 需要令牌鉴权，客户端自己下不了 ——
	// 所以无论哪种格式，都得先由网关带凭据下下来。
	data := make([]map[string]any, 0, len(resp.Images))
	for _, u := range resp.Images {
		raw, mime, err := h.fetchUpstreamImage(ctx, up, preq.Credential, u)
		if err != nil {
			writeAPIError(w, &APIError{
				Code: http.StatusBadGateway,
				Message: fmt.Sprintf("图片已生成但取回失败：%v", err),
				Type: "upstream_error",
			})
			return
		}
		if format == "url" {
			// 显式要 url 时才给地址（少数客户端只认这个字段）。
			// 给的是**本网关**的地址，不是上游那个需要鉴权的临时地址。
			id := h.images.put(raw, mime)
			data = append(data, map[string]any{"url": imageURL(r, id)})
			continue
		}
		// 默认：直接给图片本体。
		data = append(data, map[string]any{
			"b64_json": base64.StdEncoding.EncodeToString(raw),
			"mime_type": mime,
		})
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"created": time.Now().Unix(),
		"data":    data,
	})
}

// maxImageBytes 是单张图片的下载上限。
//
// 20MB：上游出图通常几百 KB 到几 MB，超过这个量级要么是异常响应，
// 要么是客户端拿到也用不了，没必要把 NAS 的内存吃掉。
const maxImageBytes = 20 << 20

// fetchUpstreamImage 把上游的图片取回来。
//
// 走 provider 的 ImageFetcher 而不是自己发请求：上游的图片地址需要
// 该账号的令牌鉴权，而令牌怎么用只有 provider 知道。
func (h *Handler) fetchUpstreamImage(ctx context.Context, up provider.Provider,
	cred any, rawURL string) ([]byte, string, error) {

	// 上游偶尔直接给 data URL（内联图片），那就没必要再下一遍。
	if strings.HasPrefix(rawURL, "data:") {
		i := strings.Index(rawURL, ",")
		if i < 0 {
			return nil, "", fmt.Errorf("data URL 格式异常")
		}
		raw, err := base64.StdEncoding.DecodeString(rawURL[i+1:])
		if err != nil {
			return nil, "", err
		}
		mime := strings.TrimPrefix(rawURL[:i], "data:")
		mime = strings.TrimSuffix(mime, ";base64")
		return raw, mime, nil
	}

	fetcher, ok := up.(provider.ImageFetcher)
	if !ok {
		return nil, "", fmt.Errorf("%w：上游 %s 不支持取回图片", provider.ErrUnsupported, up.Name())
	}
	return fetcher.FetchImage(ctx, cred, rawURL)
}

// userAgentForGateway 是网关对外发起请求时的标识。
const userAgentForGateway = "Agg-API-DSM/1.0"
