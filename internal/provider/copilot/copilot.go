// Package copilot 是 M365 Copilot 上游的实现。
//
// 迁移来源：M365-Copilot2API 的 internal/{auth,chathub,outbound}。
// 其中 auth 的「设备码授权」按用户要求**不迁移**（用不上）。
//
// 与 Agnes 上游的关键差异：Copilot 走 WebSocket 长连接，且连接有生命周期，
// 因此需要连接池（chathub 包）。这部分无法用标准库替代，
// 是本项目保留 gorilla/websocket 的唯一原因。
//
// 账号池、限流、熔断与 Agnes 共用同一份 internal/pool 实现 ——
// 这正是「互补而非重叠」的落点。
package copilot

import (
	"context"
	"errors"
	"fmt"
	"log"
	"strings"
	"time"

	"aggapi/internal/config"
	"aggapi/internal/pool"
	"aggapi/internal/provider"
	"aggapi/internal/provider/copilot/auth"
	"aggapi/internal/provider/copilot/chathub"
)

// Provider 实现 provider.Provider。
type Provider struct {
	store  *config.Store
	pool   *pool.Pool
	client *chathub.Client
	login  *Login
}

// New 构造 Copilot 上游。
//
// 账号池与 Agnes 那边是同一个实现（internal/pool）—— 这正是本项目
// 「互补而非重叠」的落点：Copilot 也有配额、也会 429、也需要冷却轮换。
func New(store *config.Store) *Provider {
	p := &Provider{
		store:  store,
		pool:   pool.New("copilot"),
		client: chathub.NewClient(),
		login:  NewLogin(),
	}
	p.pool.Sync(store)
	return p
}

// Login 暴露授权流程给控制台。
func (p *Provider) Login() *Login { return p.login }

// AuthStart 发起一次授权。
func (p *Provider) AuthStart() (provider.AuthStart, error) {
	r, err := p.login.Start()
	if err != nil {
		return provider.AuthStart{}, err
	}
	return provider.AuthStart{
		AuthURL:     r.AuthURL,
		State:       r.State,
		RedirectURI: r.RedirectURI,
		Hint:        r.Hint,
		ExpiresIn:   r.ExpiresIn,
	}, nil
}

// AuthFinish 用回调地址换取凭据、落库，并让账号池立刻认得这个新账号。
//
// 必须调 Sync：否则用户加完账号要重启服务才能用上，
// 那种「明明加成功了却还是说没有账号」的体验很糟。
func (p *Provider) AuthFinish(state, pasted, displayName string) (string, string, error) {
	acct, err := p.login.Finish(state, pasted, displayName)
	if err != nil {
		return "", "", err
	}
	saved, err := p.store.UpsertAccount(acct)
	if err != nil {
		return "", "", fmt.Errorf("保存账号失败：%w", err)
	}
	p.Sync()
	return saved.ID, saved.Name, nil
}

// refreshSkew 是提前刷新的余量。
//
// 留 5 分钟而不是卡着过期时间刷：一次对话可能跑很久（生图尤其慢），
// 令牌在请求途中过期会让整次调用白费。
const refreshSkew = 5 * time.Minute

// ensureFresh 在令牌临近过期时先刷新，并把新令牌写回配置。
//
// 刷新失败不直接报错，而是让请求继续用旧令牌试一次 ——
// 上游偶尔会在令牌「看似过期」时仍然接受，白白失败一次不划算。
func (p *Provider) ensureFresh(ctx context.Context, acct config.Account) config.Account {
	exp, err := time.Parse(time.RFC3339, acct.Auth["expires_at"])
	if err != nil {
		return acct // 没有过期信息：不动它，让上游自己判断
	}
	if time.Until(exp) > refreshSkew {
		return acct
	}

	rt := strings.TrimSpace(acct.Auth["refresh_token"])
	if rt == "" {
		return acct
	}

	ts, err := auth.Refresh(rt, "", "", acct.Auth["oid"], acct.Auth["tid"])
	if err != nil {
		log.Printf("[copilot] 账号 %s 刷新令牌失败，沿用旧令牌：%v", acct.Name, err)
		return acct
	}

	acct.Auth["access_token"] = ts.AccessToken
	if ts.RefreshToken != "" {
		// 微软会轮换 refresh_token，不保存新的下次就刷不动了。
		acct.Auth["refresh_token"] = ts.RefreshToken
	}
	if ts.HomeOID != "" {
		acct.Auth["oid"] = ts.HomeOID
	}
	if ts.TenantID != "" {
		acct.Auth["tid"] = ts.TenantID
	}
	acct.Auth["expires_at"] = ts.ExpiresAt.Format(time.RFC3339)

	if _, err := p.store.UpsertAccount(acct); err != nil {
		log.Printf("[copilot] 账号 %s 新令牌落盘失败：%v", acct.Name, err)
	}
	return acct
}

// Sync 重新读取配置。
func (p *Provider) Sync() { p.pool.Sync(p.store) }

// PoolStats 暴露账号池状态给控制台。
func (p *Provider) PoolStats() []pool.AccountStat { return p.pool.Stats() }

// Revive 手动复活一个冷却中的账号。
func (p *Provider) Revive(id string) bool { return p.pool.Revive(id) }

// Acquire 从账号池领一个账号。
func (p *Provider) Acquire(ctx context.Context, sessionKey string) (provider.Lease, error) {
	lease, err := p.pool.Acquire(ctx, sessionKey, 0, 0)
	if err != nil {
		return nil, fmt.Errorf("%w：%s 没有可用账号（%v）",
			provider.ErrNoCapacity, p.DisplayName(), err)
	}
	return lease, nil
}

func (p *Provider) Name() string        { return "copilot" }
func (p *Provider) DisplayName() string { return "M365 Copilot" }

// Models 返回 Copilot 侧对外暴露的模型。
//
// 命名前缀 copilot- 与 Agnes 侧的 agnes- 刻意区分开：
// 模型名是路由的唯一依据，前缀让用户在客户端里一眼看出走的是哪条链路。
func (p *Provider) Models() []provider.Model {
	// 没配账号时也列出模型，让用户能在客户端里先看到链路存在，
	// 而不是连模型名都找不到、不知道该填什么。
	return []provider.Model{
		{
			ID:       "copilot-auto",
			Upstream: "auto",
			Caps: provider.CapText | provider.CapImage | provider.CapVision |
				provider.CapDoc | provider.CapStream,
			Desc: "Copilot 智能路由（含生图与文档解析）",
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

// Health 用账号池的真实状态报告健康。
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

// toChatHubAccount 把配置里的账号翻译成 chathub 需要的凭据。
//
// 凭据放在 config.Account.Auth 这个 map 里，而不是给 Account 结构加
// Copilot 专属字段 —— 账号池要能一视同仁地处理两个上游的账号。
func toChatHubAccount(a config.Account) (chathub.Account, error) {
	token := strings.TrimSpace(a.Auth["access_token"])
	if token == "" {
		return chathub.Account{}, fmt.Errorf(
			"%w：账号 %s 缺少 access_token，请在控制台重新授权", provider.ErrAuth, a.Name)
	}
	return chathub.Account{
		AccessToken: token,
		OID:         a.Auth["oid"],
		TID:         a.Auth["tid"],
	}, nil
}

// Chat 执行一次对话。
func (p *Provider) Chat(ctx context.Context, req *provider.ChatRequest) (*provider.ChatResponse, error) {
	acct, ok := req.Credential.(config.Account)
	if !ok {
		return nil, fmt.Errorf("%w：Copilot 未收到账号凭据", provider.ErrNoCapacity)
	}
	// 令牌可能已过期：先刷新再建连，避免请求跑到一半被上游断掉。
	acct = p.ensureFresh(ctx, acct)
	ca, err := toChatHubAccount(acct)
	if err != nil {
		return nil, err
	}

	creq := chathub.Request{
		Text:   joinMessages(req.Messages),
		Locale: "zh-CN",
		Market: "zh-CN",
		// 模型名决定场景：copilot-image 走生图，其余走对话。
		// 上游用同一个 WebSocket 端点承载两种请求，靠 scenario 区分。
		Scenario: scenarioFor(req.Model),
	}

	res, err := p.client.Chat(ctx, ca, creq)
	if err != nil {
		return nil, classifyChatErr(err)
	}

	// 去掉正文里的引用标记（[^1^] 之类），并把被引用的 URL 附在末尾 ——
	// 客户端看不到 Copilot 的引用面板，不附上等于信息丢失。
	text, citedURLs := chathub.StripCitationMarkers(res.Text, res.References)
	if len(citedURLs) > 0 {
		text += "\n\n参考来源：\n"
		for i, u := range citedURLs {
			text += fmt.Sprintf("%d. %s\n", i+1, u)
		}
	}

	out := &provider.ChatResponse{
		Model:   req.Model,
		Content: strings.TrimRight(text, "\n"),
		Images:  res.Images,
	}
	return out, nil
}

// classifyChatErr 把 chathub 的错误映射到 provider 的哨兵错误。
//
// 这层映射决定了下游看到的状态码：鉴权过期值得让用户去重新授权（502），
// 配额不足值得让客户端稍后重试（503）。混在一起会让用户无从下手。
func classifyChatErr(err error) error {
	var de *chathub.DialError
	if errors.As(err, &de) {
		switch {
		case de.RetryAfter > 0:
			return fmt.Errorf("%w：上游限流，建议 %d 秒后重试", provider.ErrNoCapacity, de.RetryAfter)
		default:
			return fmt.Errorf("%w：%v", provider.ErrUpstream, err)
		}
	}
	msg := err.Error()
	if chathub.IsContentPolicyBlock(msg) {
		return fmt.Errorf("%w：请求被上游内容策略拦截", provider.ErrUnsupported)
	}
	if strings.Contains(msg, "401") || strings.Contains(msg, "403") ||
		strings.Contains(strings.ToLower(msg), "unauthorized") {
		return fmt.Errorf("%w：%v", provider.ErrAuth, err)
	}
	return fmt.Errorf("%w：%v", provider.ErrUpstream, err)
}

// scenarioFor 把模型名映射成上游的 scenario 参数。
func scenarioFor(model string) string {
	switch {
	case strings.Contains(model, "image"):
		return "image"
	default:
		return "chat"
	}
}

// joinMessages 把归一化消息拼成上游要的单段文本。
//
// Copilot 的 WebSocket 协议一次只接受一段提示词，没有多轮消息数组；
// 多轮上下文要靠 ConversationID 维持。所以这里把历史消息按角色前缀拼接 ——
// 虽然不如原生多轮精确，但能保证语义不丢。
func joinMessages(msgs []provider.Message) string {
	if len(msgs) == 0 {
		return ""
	}
	// 只有一条 user 消息时直接用原文，不加前缀 ——
	// 加前缀会污染提示词，影响模型对指令的理解。
	if len(msgs) == 1 && msgs[0].Role == "user" {
		return msgs[0].Content
	}
	var sb strings.Builder
	for _, m := range msgs {
		switch m.Role {
		case "system":
			sb.WriteString("[系统] ")
		case "assistant":
			sb.WriteString("[助手] ")
		default:
			sb.WriteString("[用户] ")
		}
		sb.WriteString(m.Content)
		sb.WriteString("\n")
	}
	return strings.TrimSpace(sb.String())
}

// ChatStream 执行一次流式对话。
//
// 尚未接通：chathub 已提供 ChatWithDelta，但网关层的 SSE 输出还没做。
// 明确返回错误而不是静默退化成一次性返回 —— 后者会让客户端的流式解析
// 收到一个不符合预期的响应。
func (p *Provider) ChatStream(ctx context.Context, req *provider.ChatRequest, ch chan<- provider.StreamChunk) {
	defer close(ch)
	ch <- provider.StreamChunk{Err: fmt.Errorf("%w：流式尚未接通", provider.ErrUnsupported)}
}
