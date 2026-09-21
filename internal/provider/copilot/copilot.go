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
	"encoding/base64"
	"encoding/json"
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

func (p *Provider) Name() string        { return "copilot" }
func (p *Provider) DisplayName() string { return "M365 Copilot" }

// Models 返回 Copilot 侧对外暴露的模型。
//
// 用微软那边的真实叫法（gpt-5.5 / claude-sonnet / ...），不用自造名 ——
// 客户端里看到 copilot-chat 这种名字，没人知道它对应哪个模型。
func (p *Provider) Models() []provider.Model { return models() }

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

// 上游要求的默认参数。这三个**必须填**，留空会被上游直接拒绝 ——
// 上游协议里没有「默认值」的概念，它期待客户端每次都显式声明。
//
// 取值对齐 M365-Copilot2API 的 settings 默认值：
//
//	Scenario    = "OfficeWebIncludedCopilot"
//	LicenseType = "Starter"
//	Tone        = "magic"（智能路由，由 modelTone 兜底）
const (
	defaultScenario    = "OfficeWebIncludedCopilot"
	defaultLicenseType = "Starter"
	defaultTone        = "magic"
)


// extractOIDTID 从 access_token 里解出 oid / tid。
//
// 上游要求这两个字段必填，但用户手工粘贴令牌时未必知道它们是什么。
// access_token 是 JWT，负载里就带着 —— 解出来比让用户去别处找要好。
func extractOIDTID(accessToken string) (oid, tid string) {
	parts := strings.Split(accessToken, ".")
	if len(parts) < 2 {
		return "", ""
	}
	raw, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return "", ""
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		return "", ""
	}
	if v, ok := m["oid"].(string); ok {
		oid = v
	}
	if v, ok := m["tid"].(string); ok {
		tid = v
	}
	return oid, tid
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
	oid := strings.TrimSpace(a.Auth["oid"])
	tid := strings.TrimSpace(a.Auth["tid"])
	if oid == "" || tid == "" {
		// 兜底：从令牌里解。上游把 oid/tid 当必填，缺了会直接拒请求，
		// 而这个错误在上游侧的表象很难懂，不如在这里自己补上。
		if o, t := extractOIDTID(token); o != "" {
			oid, tid = o, t
		}
	}
	if oid == "" || tid == "" {
		return chathub.Account{}, fmt.Errorf(
			"%w：账号 %s 的凭据里缺少 oid/tid，且无法从令牌中解析，请重新授权",
			provider.ErrAuth, a.Name)
	}
	return chathub.Account{AccessToken: token, OID: oid, TID: tid}, nil
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

	creq := buildRequest(req)

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

// firstNonEmptyTone 空值时回落到智能路由。
//
// 上游把 tone 当必填，空值会被拒；回落成 magic 至少能出一个合理的结果。
func firstNonEmptyTone(tone string) string {
	if strings.TrimSpace(tone) == "" {
		return defaultTone
	}
	return tone
}

// buildRequest 把归一化请求翻译成 chathub 的请求。
//
// Chat 与 ChatStream 共用 —— 两条路径的字段必须完全一致，
// 否则会出现「非流式能用、流式不能用」这种很难查的差异。
func buildRequest(req *provider.ChatRequest) chathub.Request {
	return chathub.Request{
		Text: joinMessages(req.Messages),
		// Tone 决定上游用哪个模型、多深的推理。
		//
		// req.Model 到这里已经是**上游标识（tone）**了 —— 网关按
		// provider.Model.Upstream 填的。不要再做一次名字->tone 的映射，
		// 那会把 "Gpt_5_5_Chat" 当成未知名字、退回 magic，用户选的模型就丢了。
		Tone: firstNonEmptyTone(req.Model),
		// Scenario 与 LicenseType 同样是上游的必填项，它没有默认值可用 ——
		// 留空会被直接拒，且上游返回的错误很难懂。
		Scenario:    defaultScenario,
		LicenseType: defaultLicenseType,
		Locale:      "zh-CN",
		Market:      "zh-CN",
		TimeZone:    "Asia/Shanghai",
		DeviceOS:    "Windows",
		// Started=true 表示这是新一轮对话的起始消息。
		Started: true,
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
