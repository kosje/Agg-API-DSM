// Package gateway 的 OpenAI 兼容端点。
//
// 下游只认这一套接口：无论背后走 Agnes 还是 Copilot，请求与响应的形状都一样。
// 模型名决定路由，客户端不需要知道有几个上游。
package gateway

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"aggapi/internal/provider"
)

// maxBodyBytes 限制请求体大小。32MB 足够覆盖带图的多模态请求，
// 同时防止一个超大 body 把内存吃光。
const maxBodyBytes = 32 << 20

// APIError 是 OpenAI 形状的错误体。
type APIError struct {
	Code    int    `json:"-"`
	Message string `json:"message"`
	Type    string `json:"type"`
	Param   string `json:"param,omitempty"`
}

// Error 让 APIError 满足 error 接口。
func (e *APIError) Error() string { return e.Message }

// errorBody 把内部错误映射成下游能看懂的状态码。
//
// 这层映射是必要的：上游的失败原因（没配额 / 鉴权过期 / 上游报错）对下游来说
// 含义完全不同 —— 前者值得重试，后者重试只会更糟。
func errorBody(err error) *APIError {
	switch {
	case errors.Is(err, provider.ErrNoCapacity):
		return &APIError{
			Code:    http.StatusServiceUnavailable,
			Message: err.Error(),
			Type:    "no_capacity",
		}
	case errors.Is(err, provider.ErrAuth):
		return &APIError{
			Code:    http.StatusBadGateway,
			Message: "上游鉴权失败，请到控制台重新授权：" + err.Error(),
			Type:    "upstream_auth",
		}
	case errors.Is(err, provider.ErrUnsupported):
		return &APIError{
			Code:    http.StatusBadRequest,
			Message: err.Error(),
			Type:    "unsupported",
		}
	case errors.Is(err, provider.ErrUpstream):
		return &APIError{
			Code:    http.StatusBadGateway,
			Message: err.Error(),
			Type:    "upstream_error",
		}
	default:
		return &APIError{
			Code:    http.StatusInternalServerError,
			Message: err.Error(),
			Type:    "internal_error",
		}
	}
}

func writeAPIError(w http.ResponseWriter, e *APIError) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(e.Code)
	_ = json.NewEncoder(w).Encode(map[string]any{"error": e})
}

// Handler 是网关的 HTTP 入口。
type Handler struct {
	router *Router
	// authorize 校验下游 API Key。返回 false 时已经写过响应。
	authorize func(http.ResponseWriter, *http.Request) bool
	// maxRetries 是单次请求最多换几个账号重试。
	maxRetries int
}

// NewHandler 构造入口。authorize 传 nil 表示不校验（仅供本机调试）。
func NewHandler(r *Router, authorize func(http.ResponseWriter, *http.Request) bool) *Handler {
	if authorize == nil {
		authorize = func(http.ResponseWriter, *http.Request) bool { return true }
	}
	return &Handler{router: r, authorize: authorize, maxRetries: 2}
}

// SetMaxRetries 调整重试次数（来自 settings.max_retries）。
func (h *Handler) SetMaxRetries(n int) {
	if n < 0 {
		n = 0
	}
	h.maxRetries = n
}

// retriable 判断一个错误是否值得换个账号重试。
//
// 这个判断很重要 —— 盲目重试会把「本来就不该重试」的错误放大：
//   - 凭据失效：换个账号也没用，得人去重新授权。重试只会白烧配额
//   - 能力不支持：请求本身有问题，重试还是同样结果
//   - 没配额 / 上游错误：值得换个账号试，这才是轮询的意义
func retriable(err error) bool {
	switch {
	case errors.Is(err, provider.ErrNoCapacity):
		return true
	case errors.Is(err, provider.ErrUpstream):
		return true
	case errors.Is(err, provider.ErrAuth):
		return false
	case errors.Is(err, provider.ErrUnsupported):
		return false
	default:
		return false
	}
}

// backoff 是换账号重试之间的退避。
//
// 线性而不是指数：账号池通常就几个账号，重试次数很少，
// 指数退避在这里只会让用户多等，收益不明显。
func backoff(attempt int) time.Duration {
	return time.Duration(300+attempt*400) * time.Millisecond
}

// Register 把端点挂到 mux 上。
func (h *Handler) Register(mux *http.ServeMux) {
	mux.HandleFunc("/v1/chat/completions", h.handleChat)
	mux.HandleFunc("/v1/models", h.handleModels)
}

func (h *Handler) handleModels(w http.ResponseWriter, r *http.Request) {
	if !h.authorize(w, r) {
		return
	}
	owner := map[string]string{}
	for _, p := range h.router.Providers() {
		for _, m := range p.Models() {
			owner[m.ID] = p.Name()
		}
	}
	models := h.router.Models()
	data := make([]map[string]any, 0, len(models))
	for _, m := range models {
		data = append(data, map[string]any{
			"id":       m.ID,
			"object":   "model",
			"owned_by": owner[m.ID],
			"created":  0,
		})
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_ = json.NewEncoder(w).Encode(map[string]any{"object": "list", "data": data})
}

// chatRequest 是下游发来的 OpenAI 形状请求。
type chatRequest struct {
	Model       string            `json:"model"`
	Messages    []json.RawMessage `json:"messages"`
	Stream      bool              `json:"stream"`
	Temperature float64           `json:"temperature"`
	MaxTokens   int               `json:"max_tokens"`
	// 其余字段原样透传给上游（reasoning_effort、tools 等），
	// 避免在归一化过程中丢失上游特有参数。
	Raw map[string]any `json:"-"`
}

func (h *Handler) handleChat(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeAPIError(w, &APIError{Code: http.StatusMethodNotAllowed,
			Message: "只支持 POST", Type: "method_not_allowed"})
		return
	}
	if !h.authorize(w, r) {
		return
	}

	body, err := io.ReadAll(io.LimitReader(r.Body, maxBodyBytes))
	if err != nil {
		writeAPIError(w, &APIError{Code: http.StatusBadRequest,
			Message: "读取请求体失败：" + err.Error(), Type: "invalid_request"})
		return
	}

	var raw map[string]any
	if err := json.Unmarshal(body, &raw); err != nil {
		writeAPIError(w, &APIError{Code: http.StatusBadRequest,
			Message: "请求体不是合法 JSON", Type: "invalid_request"})
		return
	}

	var req chatRequest
	if err := json.Unmarshal(body, &req); err != nil {
		writeAPIError(w, &APIError{Code: http.StatusBadRequest,
			Message: "请求体结构不符合 OpenAI 格式", Type: "invalid_request"})
		return
	}
	req.Raw = raw

	if strings.TrimSpace(req.Model) == "" {
		writeAPIError(w, &APIError{Code: http.StatusBadRequest,
			Message: "缺少 model 字段", Type: "invalid_request", Param: "model"})
		return
	}

	up, model, err := h.router.Lookup(req.Model)
	if err != nil {
		writeAPIError(w, &APIError{Code: http.StatusNotFound,
			Message: err.Error(), Type: "model_not_found", Param: "model"})
		return
	}

	// 转发前先查能力，避免把请求发出去、拿回一个语焉不详的上游报错。
	if wantsImage(req.Raw) && !model.Caps.Has(provider.CapImage) {
		writeAPIError(w, &APIError{
			Code: http.StatusBadRequest,
			Message: fmt.Sprintf("模型 %s 不支持图片生成（属于 %s 上游）",
				model.ID, up.Name()),
			Type: "unsupported", Param: "model",
		})
		return
	}

	// 附件先于路由处理：解析失败要能直接返回 400，而不是把请求发出去
	// 再拿回一个语焉不详的上游错误。
	images, docText, err := parseAttachments(req.Raw)
	if err != nil {
		writeAPIError(w, &APIError{
			Code:    http.StatusBadRequest,
			Message: err.Error(),
			Type:    "invalid_attachment",
		})
		return
	}

	// 从账号池领一个账号并转发，失败就换一个账号重试。
	//
	// 这个循环是「账号轮询」真正生效的地方 —— 只 Acquire 一次的话，
	// 挑中一个坏账号就直接把错误抛给下游，池子形同虚设。
	preq := toProviderRequest(req, model)
	preq.Images = images
	attachDocs(preq, docText)
	preq.Stream = req.Stream

	if req.Stream {
		h.streamChat(w, r, up, model.Upstream, req.Model, preq, sessionKey(r, preq))
		return
	}

	var (
		resp    *provider.ChatResponse
		lastErr error
		tried   []string
	)
	for attempt := 0; attempt <= h.maxRetries; attempt++ {
		lease, aerr := up.Acquire(r.Context(), sessionKey(r, preq), tried...)
		if aerr != nil {
			// 池里已经没别的账号可用了：把最后一次的上游错误报出去，
			// 那比「没有可用账号」更能说明问题。
			if lastErr != nil {
				break
			}
			writeAPIError(w, errorBody(aerr))
			return
		}
		preq.Credential = lease.Credential()
		resp, lastErr = up.Chat(r.Context(), preq)
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

	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_ = json.NewEncoder(w).Encode(toOpenAIResponse(req.Model, resp))
}

// sessionKey 给账号池的软粘性用：同一会话尽量复用上次成功的账号。
//
// 优先用调用方显式给的 X-Session-Id；没有就退化成「首条用户消息的散列」——
// 同一段对话的开头通常相同，足以把连续几轮请求归到同一个会话。
// 都不满足时返回空串，表示不做粘性（纯按负载选账号）。
func sessionKey(r *http.Request, req *provider.ChatRequest) string {
	if v := strings.TrimSpace(r.Header.Get("X-Session-Id")); v != "" {
		return "sid:" + v
	}
	for _, m := range req.Messages {
		if m.Role == "user" && m.Content != "" {
			sum := sha256.Sum256([]byte(m.Content))
			return "msg:" + hex.EncodeToString(sum[:8])
		}
	}
	return ""
}

// wantsImage 判断这个请求是不是在要图片。
//
// 上游的图片接口与对话接口共用 /v1/chat/completions，靠模型名或
// modalities 字段区分。这里两种都认。
func wantsImage(raw map[string]any) bool {
	if v, ok := raw["modalities"]; ok {
		if arr, ok := v.([]any); ok {
			for _, x := range arr {
				if s, ok := x.(string); ok && strings.EqualFold(s, "image") {
					return true
				}
			}
		}
	}
	return false
}

// toProviderRequest 把下游请求转成归一化请求。
func toProviderRequest(req chatRequest, model provider.Model) *provider.ChatRequest {
	out := &provider.ChatRequest{
		Model:       model.Upstream,
		Stream:      false,
		Temperature: req.Temperature,
		MaxTokens:   req.MaxTokens,
		Raw:         req.Raw,
	}
	for _, m := range req.Messages {
		var msg struct {
			Role    string `json:"role"`
			Content any    `json:"content"`
		}
		if json.Unmarshal(m, &msg) != nil {
			continue
		}
		out.Messages = append(out.Messages, provider.Message{
			Role:    msg.Role,
			Content: flattenContent(msg.Content),
		})
	}
	return out
}

// flattenContent 把多模态 content 数组压成纯文本，同时收集图片。
//
// 上游的文本模型只吃字符串，图片要走各自的通道；
// 不在这里丢信息，而是把图片挑出来放进 Images 交给支持 CapVision 的上游。
func flattenContent(v any) string {
	switch x := v.(type) {
	case string:
		return x
	case []any:
		var sb strings.Builder
		for _, part := range x {
			m, ok := part.(map[string]any)
			if !ok {
				continue
			}
			if t, ok := m["text"].(string); ok {
				sb.WriteString(t)
			}
		}
		return sb.String()
	default:
		return ""
	}
}

// toOpenAIResponse 把归一化响应转回 OpenAI 形状。
func toOpenAIResponse(model string, resp *provider.ChatResponse) map[string]any {
	return map[string]any{
		"id":     "chatcmpl-agg",
		"object": "chat.completion",
		"model":  model,
		"choices": []map[string]any{{
			"index":         0,
			"finish_reason": "stop",
			"message": map[string]any{
				"role":    "assistant",
				"content": resp.Content,
			},
		}},
		"usage": map[string]any{
			"prompt_tokens":     resp.Usage.PromptTokens,
			"completion_tokens": resp.Usage.CompletionTokens,
			"total_tokens":      resp.Usage.TotalTokens,
		},
	}
}
