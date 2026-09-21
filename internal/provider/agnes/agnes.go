// Package agnes 是 Agnes AI 上游的实现。
//
// 迁移来源：agnes-hub-go 的 internal/relay（协议适配）+ internal/pool（账号选择）。
// 原项目把「选账号」与「转发」揉在 relay.Do 里；新架构把前者上提到网关层，
// 本包只做纯粹的协议适配 —— 给定账号与请求，构造上游请求并解析响应。
//
// 账号池与限流由网关层统一处理，本包不重复实现。
package agnes

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"aggapi/internal/config"
	"aggapi/internal/pool"
	"aggapi/internal/provider"
)

// Agnes 的两个官方站点。
//
// 注意域名是 **agnes-ai.com** / **agnes-ai.cn**，不是 agnes.ai ——
// 后者根本不存在（写错的话所有请求都会 DNS 解析失败，
// 而错误信息里只看到「no such host」，不容易联想到是默认值写错了）。
const (
	// DefaultBaseURL 是账号未填 base_url 时用的国际站。
	DefaultBaseURL = "https://apihub.agnes-ai.com/v1"
	// CNBaseURL 是中国站。两个站点账号互通，但凭据被拒的情况可能只在一侧。
	CNBaseURL = "https://api.agnes-ai.cn/v1"
)

// IsCNHost 判断这个 base_url 是不是中国站。
func IsCNHost(baseURL string) bool {
	return strings.Contains(strings.ToLower(baseURL), "agnes-ai.cn")
}

// altSiteFor 返回「同一账号的另一个站点」地址。
//
// 用途：某个站点返回 401/403 时，换另一站点重试同一账号。
// 两个站点凭据互通，被拒常常是单侧的风控或路由问题，
// 换一边往往就好了 —— 这是 agnes 原项目 RegionPriority 的语义。
func altSiteFor(baseURL string) string {
	if IsCNHost(baseURL) {
		return DefaultBaseURL
	}
	return CNBaseURL
}

// userAgent 固定标识自己。
//
// 上游会按 UA 做风控，伪装成浏览器反而更容易被判定异常，
// 如实标注来源更稳。
const userAgent = "Agg-API-DSM/1.0"

// hopByHop 是不能透传到上游的逐跳首部。
var hopByHop = map[string]bool{
	"connection": true, "keep-alive": true, "proxy-authenticate": true,
	"proxy-authorization": true, "te": true, "trailer": true,
	"transfer-encoding": true, "upgrade": true,
}

// Provider 实现 provider.Provider。
type Provider struct {
	store  *config.Store
	client *http.Client
	// pool 是账号池。它是两个上游共享的实现（internal/pool），
	// 本包只负责把它接进来，不重复实现轮换 / 限流 / 熔断。
	pool *pool.Pool
}

func New(store *config.Store) *Provider {
	transport := &http.Transport{
		MaxIdleConns:        200,
		MaxIdleConnsPerHost: 60,
		IdleConnTimeout:     90 * time.Second,
		ForceAttemptHTTP2:   true,
	}
	p := &Provider{
		store: store,
		pool:  pool.New("agnes"),
		client: &http.Client{
			Transport: transport,
			// 不跟重定向：上游用 3xx 表达「换站点」或鉴权问题，
			// 自动跟随会把错误掩盖成一个更难查的 404。
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
	}
	p.pool.Sync(store)
	go p.refreshModels()
	return p
}

// Sync 重新读取配置。
func (p *Provider) Sync() {
	p.pool.Sync(p.store)
	// 配置变了（可能换了账号或 Key），顺手重查一次上游模型清单。
	// 不阻塞调用方：拉清单要走网络，不该让保存配置的请求等它。
	go p.refreshModels()
}

// PoolStats 暴露账号池状态给控制台。
func (p *Provider) PoolStats() []pool.AccountStat { return p.pool.Stats() }

// Revive 手动复活一个冷却中的账号。
func (p *Provider) Revive(id string) bool { return p.pool.Revive(id) }

// Acquire 从账号池领一个账号。
//
// 失败信息里带上上游名：用户配了 Agnes 但没配 Copilot 时，
// 光看到「没有可用账号」无法判断该去哪个页面补配置。
func (p *Provider) Acquire(ctx context.Context, sessionKey string, exclude ...string) (provider.Lease, error) {
	lease, err := p.pool.Acquire(ctx, sessionKey, 0, 0, exclude...)
	if err != nil {
		// 不再把内层 err 也拼进去：它是 "没有可用账号"，
		// 和这里的措辞重复，读起来像 "没有可用账号（没有可用账号）"。
		return nil, fmt.Errorf("%w：%s %s",
			provider.ErrNoCapacity, p.DisplayName(), err)
	}
	return lease, nil
}

// AccountStats 实现 provider.PoolReporter，供控制台展示池状态。
func (p *Provider) AccountStats() []provider.AccountStat {
	stats := p.pool.Stats()
	out := make([]provider.AccountStat, 0, len(stats))
	for _, s := range stats {
		item := provider.AccountStat{
			ID:          s.ID,
			Name:        s.Name,
			Healthy:     s.Healthy,
			Strikes:     s.Strikes,
			RPM:         int(s.RPM),
			Waiting:     s.Waiting,
			InFlight:    s.InFlight,
			CooldownSec: int(s.Cooldown / 1e9),
			Requests:    s.Stats.Requests,
			Errors:      s.Stats.Errors,
			RateLimited: s.Stats.RateLimited,
			AuthFailed:  s.Stats.AuthFailed,
			LastError:   s.Stats.LastError,
		}
		if !s.Stats.LastUsedAt.IsZero() {
			item.LastUsedAt = s.Stats.LastUsedAt.Format(time.RFC3339)
		}
		out = append(out, item)
	}
	return out
}

// ReviveAccount 实现 provider.PoolReporter。
func (p *Provider) ReviveAccount(id string) bool { return p.pool.Revive(id) }

// ResetAccountStats 实现 provider.PoolReporter。
func (p *Provider) ResetAccountStats(id string) bool { return p.pool.ResetStats(id) }

func (p *Provider) Name() string        { return "agnes" }
func (p *Provider) DisplayName() string { return "Agnes AI" }



// Health 用账号池的真实状态报告健康，而不是只数账号个数 ——
// 「配了 5 个账号但 5 个都在熔断」和「1 个账号健康」是完全不同的状态，
// 控制台上必须能区分。
func (p *Provider) Health() provider.Health {
	ok, total := p.pool.Healthy()
	if total == 0 {
		return provider.Health{Ready: false, Detail: "尚未配置账号"}
	}
	if ok == 0 {
		return provider.Health{
			Ready:  false,
			Detail: fmt.Sprintf("%d 个账号全部冷却中", total),
		}
	}
	return provider.Health{
		Ready:  true,
		Detail: fmt.Sprintf("%d/%d 个账号可用", ok, total),
	}
}

// upstreamRoot 把账号的 base_url 归一化成「站点根」。
//
// 上游同时接受带 /v1 与不带两种写法，而端点路径本身带 /v1。
// 若直接拼接，写成 /v1 的用户会得到 /v1/v1/chat/completions ——
// 这是官方文档点名过的「第三方工具重复拼接 /v1」404 坑。
func upstreamRoot(a config.Account) string {
	base := strings.TrimRight(strings.TrimSpace(a.BaseURL), "/")
	if base == "" {
		base = DefaultBaseURL
	}
	base = strings.TrimSuffix(base, "/v1")
	return strings.TrimRight(base, "/")
}

func upstreamURL(a config.Account, path string) string {
	root := upstreamRoot(a)
	if strings.HasPrefix(path, "/") {
		return root + path
	}
	return root + "/" + path
}

// Chat 执行一次对话。
//
// 账号由网关从账号池领出后放进 req.Credential —— 本函数不自己选账号，
// 那是共享能力的职责。这里只做协议适配：给定账号与请求，转发并解析。
func (p *Provider) Chat(ctx context.Context, req *provider.ChatRequest) (*provider.ChatResponse, error) {
	acct, ok := req.Credential.(config.Account)
	if !ok {
		return nil, fmt.Errorf("%w：Agnes 未收到账号凭据", provider.ErrNoCapacity)
	}
	return p.chatWith(ctx, acct, req)
}

// chatWith 用指定账号转发一次请求。
func (p *Provider) chatWith(ctx context.Context, acct config.Account,
	req *provider.ChatRequest) (*provider.ChatResponse, error) {

	body, err := p.buildBody(req)
	if err != nil {
		return nil, err
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost,
		upstreamURL(acct, "/v1/chat/completions"), bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	httpReq.Header.Set("Authorization", "Bearer "+acct.APIKey)
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Accept", "application/json")
	httpReq.Header.Set("User-Agent", userAgent)

	resp, err := p.client.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("%w：%v", provider.ErrUpstream, err)
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
	if err != nil {
		return nil, fmt.Errorf("%w：读取响应失败：%v", provider.ErrUpstream, err)
	}

	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		return nil, fmt.Errorf("%w：账号 %s 鉴权失败（HTTP %d）",
			provider.ErrAuth, acct.Name, resp.StatusCode)
	}
	if resp.StatusCode == http.StatusTooManyRequests {
		return nil, fmt.Errorf("%w：账号 %s 被限流", provider.ErrNoCapacity, acct.Name)
	}
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("%w：HTTP %d %s",
			provider.ErrUpstream, resp.StatusCode, truncate(string(raw), 300))
	}

	return parseResponse(req.Model, raw)
}

// buildBody 把归一化请求还原成上游认的 OpenAI 形状。
func (p *Provider) buildBody(req *provider.ChatRequest) ([]byte, error) {
	// 若调用方给了原始请求体，以它为基础改写模型名 ——
	// 这样上游特有的字段（reasoning_effort、tool_choice 等）不会在归一化中丢失。
	body := map[string]any{}
	for k, v := range req.Raw {
		body[k] = v
	}
	// agnes-auto 是聚合别名，上游不认识它，必须在这里换成真实模型名。
	body["model"] = p.resolveAuto(req.Model)

	// messages 一律用归一化后的那份重建，**不**沿用 Raw 里的。
	//
	// Raw 是透传原始请求体用的，但它会把网关对消息做过的加工全部覆盖掉 ——
	// 附件解析出来的文本、多模态内容的拍平，都会在这里被悄悄丢掉。
	// 这个 bug 很难发现：请求成功、上游也回话，只是模型完全没看到附件。
	msgs := make([]map[string]any, 0, len(req.Messages))
	for _, m := range req.Messages {
		msgs = append(msgs, map[string]any{"role": m.Role, "content": m.Content})
	}
	body["messages"] = msgs
	if req.Stream {
		body["stream"] = true
	}
	if req.Temperature > 0 {
		body["temperature"] = req.Temperature
	}
	if req.MaxTokens > 0 {
		body["max_tokens"] = req.MaxTokens
	}
	return json.Marshal(body)
}

// parseResponse 解析上游的 OpenAI 兼容响应。
func parseResponse(model string, raw []byte) (*provider.ChatResponse, error) {
	var parsed struct {
		Choices []struct {
			Message struct {
				Content any `json:"content"`
			} `json:"message"`
			Delta struct {
				Content string `json:"content"`
			} `json:"delta"`
		} `json:"choices"`
		Usage struct {
			PromptTokens     int `json:"prompt_tokens"`
			CompletionTokens int `json:"completion_tokens"`
			TotalTokens      int `json:"total_tokens"`
		} `json:"usage"`
		Error *struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return nil, fmt.Errorf("%w：响应不是合法 JSON：%s",
			provider.ErrUpstream, truncate(string(raw), 300))
	}
	if parsed.Error != nil && parsed.Error.Message != "" {
		return nil, fmt.Errorf("%w：%s", provider.ErrUpstream, parsed.Error.Message)
	}

	out := &provider.ChatResponse{Model: model}
	if len(parsed.Choices) > 0 {
		// content 可能是字符串，也可能是分段数组（多模态响应），两种都收。
		switch v := parsed.Choices[0].Message.Content.(type) {
		case string:
			out.Content = v
		case []any:
			for _, part := range v {
				if m, ok := part.(map[string]any); ok {
					if s, ok := m["text"].(string); ok {
						out.Content += s
					}
					if u, ok := m["image_url"].(string); ok {
						out.Images = append(out.Images, u)
					}
				}
			}
		}
		if out.Content == "" {
			out.Content = parsed.Choices[0].Delta.Content
		}
	}
	out.Usage = provider.Usage{
		PromptTokens:     parsed.Usage.PromptTokens,
		CompletionTokens: parsed.Usage.CompletionTokens,
		TotalTokens:      parsed.Usage.TotalTokens,
	}
	var rawMap map[string]any
	if json.Unmarshal(raw, &rawMap) == nil {
		out.Raw = rawMap
	}
	return out, nil
}

func truncate(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
