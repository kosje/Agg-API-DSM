// Package gateway 的管理 API。
//
// 全部挂在 /api/ 下，与控制台前端配套。鉴权用会话 Cookie：
// 口令只在登录那一次传输，之后靠随机会话令牌 —— 避免每个请求都带着口令。
//
// 未设置口令时只放行本机访问：首次部署要能打开控制台去设置它，
// 但绝不能因为「还没设口令」就把管理接口对整个局域网敞开。
package gateway

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"sync"
	"time"

	"aggapi/internal/config"
	"aggapi/internal/provider"
)

// sessionTTL 是管理会话有效期。
const sessionTTL = 12 * time.Hour

// Admin 提供管理 API。
type Admin struct {
	store  *config.Store
	router *Router
	// authProviders 是支持交互式授权的上游，按 Name() 索引。
	// 用 map 而不是遍历 router：控制台要按名字精确定位，
	// 且只有部分上游具备这个能力。
	authProviders map[string]provider.AuthProvider

	mu       sync.Mutex
	sessions map[string]time.Time // 令牌 -> 过期时间
}

// NewAdmin 构造管理 API。
func NewAdmin(store *config.Store, router *Router) *Admin {
	aps := map[string]provider.AuthProvider{}
	for _, p := range router.Providers() {
		if ap, ok := p.(provider.AuthProvider); ok {
			aps[p.Name()] = ap
		}
	}
	return &Admin{
		store:         store,
		router:        router,
		authProviders: aps,
		sessions:      map[string]time.Time{},
	}
}

// Register 把管理接口挂到 mux 上。
func (a *Admin) Register(mux *http.ServeMux) {
	mux.HandleFunc("/api/state", a.handleState)
	mux.HandleFunc("/api/setup", a.handleSetup)
	mux.HandleFunc("/api/login", a.handleLogin)
	mux.HandleFunc("/api/logout", a.handleLogout)
	mux.HandleFunc("/api/accounts", a.handleAccounts)
	mux.HandleFunc("/api/accounts/delete", a.handleAccountDelete)
	mux.HandleFunc("/api/keys", a.handleKeys)
	mux.HandleFunc("/api/keys/delete", a.handleKeyDelete)
	mux.HandleFunc("/api/auth/start", a.handleAuthStart)
	mux.HandleFunc("/api/auth/finish", a.handleAuthFinish)
	mux.HandleFunc("/api/diag", a.handleDiag)
	mux.HandleFunc("/api/revive", a.handleRevive)
}

// ---------- 会话 ----------

func (a *Admin) newSession() (string, error) {
	var b [32]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	tok := hex.EncodeToString(b[:])
	a.mu.Lock()
	// 顺手清理过期会话，避免 map 无限增长。
	now := time.Now()
	for k, exp := range a.sessions {
		if now.After(exp) {
			delete(a.sessions, k)
		}
	}
	a.sessions[tok] = now.Add(sessionTTL)
	a.mu.Unlock()
	return tok, nil
}

func (a *Admin) validSession(r *http.Request) bool {
	c, err := r.Cookie("agg_session")
	if err != nil || c.Value == "" {
		return false
	}
	a.mu.Lock()
	exp, ok := a.sessions[c.Value]
	a.mu.Unlock()
	return ok && time.Now().Before(exp)
}

// requireAdmin 校验管理权限。未通过时已写过响应，返回 false。
func (a *Admin) requireAdmin(w http.ResponseWriter, r *http.Request) bool {
	if a.validSession(r) {
		return true
	}
	// 还没设口令时放行本机，否则用户永远进不去设置页面。
	if !a.store.HasAdminPassword() && isLoopbackAddr(r.RemoteAddr) {
		return true
	}
	writeJSON(w, http.StatusUnauthorized, map[string]any{
		"error": "需要管理员登录",
	})
	return false
}

func isLoopbackAddr(remoteAddr string) bool {
	host, _, err := net.SplitHostPort(remoteAddr)
	if err != nil {
		host = remoteAddr
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// ---------- 接口 ----------

// handleState 返回控制台首屏需要的全部状态。
func (a *Admin) handleState(w http.ResponseWriter, r *http.Request) {
	if !a.requireAdmin(w, r) {
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"need_setup": !a.store.HasAdminPassword(),
		"accounts":   a.store.Accounts(),
		"keys":       a.store.Keys(),
		"settings":   a.store.Settings,
		"providers":  a.providerInfo(),
	})
}

// providerInfo 汇总各上游的状态，供控制台展示。
//
// 池状态（谁在冷却、排了多少队）是排查的关键 —— 只看「1/1 个账号可用」
// 看不出瓶颈在哪。这里用可选接口探测，不支持的上游就不带这个字段。
func (a *Admin) providerInfo() []map[string]any {
	out := []map[string]any{}
	for _, p := range a.router.Providers() {
		h := p.Health()
		item := map[string]any{
			"name":         p.Name(),
			"display_name": p.DisplayName(),
			"ready":        h.Ready,
			"detail":       h.Detail,
			"models":       modelIDs(p.Models()),
		}
		if _, ok := a.authProviders[p.Name()]; ok {
			item["can_auth"] = true
		}
		if pr, ok := p.(provider.PoolReporter); ok {
			item["accounts"] = pr.AccountStats()
		}
		if ms, ok := p.(provider.ModelSourceReporter); ok {
			item["model_source"] = ms.ModelSource()
		}
		out = append(out, item)
	}
	return out
}

func modelIDs(ms []provider.Model) []string {
	ids := make([]string, 0, len(ms))
	for _, m := range ms {
		ids = append(ids, m.ID)
	}
	return ids
}

// handleSetup 首次设置管理端口令。
func (a *Admin) handleSetup(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, errObj("只支持 POST"))
		return
	}
	// 已经设过口令就不能再走这个接口，否则等于给了一个绕过登录的口子。
	if a.store.HasAdminPassword() {
		writeJSON(w, http.StatusForbidden, errObj("口令已设置，请用登录接口修改"))
		return
	}
	if !isLoopbackAddr(r.RemoteAddr) {
		writeJSON(w, http.StatusForbidden, errObj("首次设置口令只能从本机进行"))
		return
	}
	var body struct {
		Password string `json:"password"`
	}
	if err := decodeBody(r, &body); err != nil {
		writeJSON(w, http.StatusBadRequest, errObj(err.Error()))
		return
	}
	if err := a.store.SetAdminPassword(body.Password); err != nil {
		writeJSON(w, http.StatusBadRequest, errObj(err.Error()))
		return
	}
	tok, err := a.newSession()
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, errObj("创建会话失败"))
		return
	}
	http.SetCookie(w, &http.Cookie{
		Name: "agg_session", Value: tok, Path: "/",
		HttpOnly: true, SameSite: http.SameSiteLaxMode, MaxAge: int(sessionTTL / time.Second),
	})
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (a *Admin) handleLogin(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, errObj("只支持 POST"))
		return
	}
	var body struct {
		Password string `json:"password"`
	}
	if err := decodeBody(r, &body); err != nil {
		writeJSON(w, http.StatusBadRequest, errObj(err.Error()))
		return
	}
	if !a.store.CheckAdminPassword(body.Password) {
		// 不区分「口令错」与「没设口令」：对外少泄漏一点信息。
		writeJSON(w, http.StatusUnauthorized, errObj("口令不正确"))
		return
	}
	tok, err := a.newSession()
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, errObj("创建会话失败"))
		return
	}
	http.SetCookie(w, &http.Cookie{
		Name: "agg_session", Value: tok, Path: "/",
		HttpOnly: true, SameSite: http.SameSiteLaxMode, MaxAge: int(sessionTTL / time.Second),
	})
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (a *Admin) handleLogout(w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie("agg_session"); err == nil {
		a.mu.Lock()
		delete(a.sessions, c.Value)
		a.mu.Unlock()
	}
	http.SetCookie(w, &http.Cookie{Name: "agg_session", Value: "", Path: "/", MaxAge: -1})
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// handleAccounts 处理账号的列出与新增/修改。
func (a *Admin) handleAccounts(w http.ResponseWriter, r *http.Request) {
	if !a.requireAdmin(w, r) {
		return
	}
	switch r.Method {
	case http.MethodGet:
		writeJSON(w, http.StatusOK, map[string]any{"accounts": a.store.Accounts()})
	case http.MethodPost:
		var acct config.Account
		if err := decodeBody(r, &acct); err != nil {
			writeJSON(w, http.StatusBadRequest, errObj(err.Error()))
			return
		}
		saved, err := a.store.UpsertAccount(acct)
		if err != nil {
			writeJSON(w, http.StatusBadRequest, errObj(err.Error()))
			return
		}
		// 必须同步池子，否则新账号不会参与调度 ——
		// 用户看到「账号列表里有、但池子里没有」，只能靠重启服务解决。
		a.syncProviders()
		writeJSON(w, http.StatusOK, map[string]any{"account": saved})
	default:
		writeJSON(w, http.StatusMethodNotAllowed, errObj("只支持 GET / POST"))
	}
}

func (a *Admin) handleAccountDelete(w http.ResponseWriter, r *http.Request) {
	if !a.requireAdmin(w, r) {
		return
	}
	id := r.URL.Query().Get("id")
	if id == "" {
		writeJSON(w, http.StatusBadRequest, errObj("缺少 id"))
		return
	}
	if err := a.store.DeleteAccount(id); err != nil {
		writeJSON(w, http.StatusNotFound, errObj(err.Error()))
		return
	}
	// 删了也要同步：不同步的话池子里还留着这个账号，
	// 请求会继续打到它，而且用户完全看不出为什么。
	a.syncProviders()
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (a *Admin) handleKeys(w http.ResponseWriter, r *http.Request) {
	if !a.requireAdmin(w, r) {
		return
	}
	switch r.Method {
	case http.MethodGet:
		writeJSON(w, http.StatusOK, map[string]any{"keys": a.store.Keys()})
	case http.MethodPost:
		var body struct {
			Name string `json:"name"`
		}
		_ = decodeBody(r, &body)
		k, plain, err := a.store.CreateKey(body.Name)
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, errObj(err.Error()))
			return
		}
		// 明文只在这里返回一次：库里存的是散列，之后再也拿不回来。
		writeJSON(w, http.StatusOK, map[string]any{"key": k, "plaintext": plain})
	default:
		writeJSON(w, http.StatusMethodNotAllowed, errObj("只支持 GET / POST"))
	}
}

func (a *Admin) handleKeyDelete(w http.ResponseWriter, r *http.Request) {
	if !a.requireAdmin(w, r) {
		return
	}
	id := r.URL.Query().Get("id")
	if id == "" {
		writeJSON(w, http.StatusBadRequest, errObj("缺少 id"))
		return
	}
	if err := a.store.DeleteKey(id); err != nil {
		writeJSON(w, http.StatusNotFound, errObj(err.Error()))
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// handleAuthStart 为某个上游发起交互式授权。
func (a *Admin) handleAuthStart(w http.ResponseWriter, r *http.Request) {
	if !a.requireAdmin(w, r) {
		return
	}
	ap, ok := a.authProviders[r.URL.Query().Get("provider")]
	if !ok {
		writeJSON(w, http.StatusNotFound, errObj("该上游不支持交互式授权"))
		return
	}
	res, err := ap.AuthStart()
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, errObj(err.Error()))
		return
	}
	writeJSON(w, http.StatusOK, res)
}

// handleAuthFinish 用用户粘贴的回调地址完成授权。
func (a *Admin) handleAuthFinish(w http.ResponseWriter, r *http.Request) {
	if !a.requireAdmin(w, r) {
		return
	}
	var body struct {
		Provider    string `json:"provider"`
		State       string `json:"state"`
		Callback    string `json:"callback"`
		DisplayName string `json:"display_name"`
	}
	if err := decodeBody(r, &body); err != nil {
		writeJSON(w, http.StatusBadRequest, errObj(err.Error()))
		return
	}
	ap, ok := a.authProviders[body.Provider]
	if !ok {
		writeJSON(w, http.StatusNotFound, errObj("该上游不支持交互式授权"))
		return
	}
	id, name, err := ap.AuthFinish(body.State, body.Callback, body.DisplayName)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, errObj(err.Error()))
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"id": id, "name": name})
}

// handleDiag 对一个上游做一次真实的自检，把完整错误原样返回。
//
// 为什么需要它：下游客户端（Cherry Studio 等）只会把失败包装成一句
// 「模型服务拒绝了测试请求」，看不出到底哪一步错了。控制台里的账号显示
// 「可用」也只说明账号没在冷却，不代表令牌有效、不代表能连上上游。
// 这个端点跑一次真实请求，把上游的原话带回来。
func (a *Admin) handleDiag(w http.ResponseWriter, r *http.Request) {
	if !a.requireAdmin(w, r) {
		return
	}
	name := r.URL.Query().Get("provider")
	model := r.URL.Query().Get("model")

	var target provider.Provider
	for _, p := range a.router.Providers() {
		if p.Name() == name {
			target = p
			break
		}
	}
	if target == nil {
		writeJSON(w, http.StatusNotFound, errObj("没有这个上游："+name))
		return
	}
	if model == "" {
		if ms := target.Models(); len(ms) > 0 {
			model = ms[0].ID
		}
	}

	out := map[string]any{
		"provider": name,
		"model":    model,
		"health":   target.Health().Detail,
	}

	ctx, cancel := context.WithTimeout(r.Context(), 90*time.Second)
	defer cancel()

	lease, err := target.Acquire(ctx, "diag")
	if err != nil {
		out["stage"] = "acquire"
		out["ok"] = false
		out["error"] = err.Error()
		writeJSON(w, http.StatusOK, out)
		return
	}

	req := &provider.ChatRequest{
		Model:      model,
		Credential: lease.Credential(),
		Messages: []provider.Message{
			{Role: "user", Content: `Say "OK" in one word.`},
		},
	}
	start := time.Now()
	resp, err := target.Chat(ctx, req)
	out["elapsed_ms"] = time.Since(start).Milliseconds()
	// 归还时故意带一个错误：自检不该把一个坏账号洗成健康的，
	// 也不该把粘性指向它。必须在请求之后归还 —— 提前归还会让账号池
	// 以为它空闲了，可能同时把这个账号再发给别的请求。
	lease.Release(errors.New("诊断请求"))
	if err != nil {
		out["stage"] = "chat"
		out["ok"] = false
		out["error"] = err.Error()
		writeJSON(w, http.StatusOK, out)
		return
	}
	out["stage"] = "done"
	out["ok"] = true
	out["reply"] = resp.Content
	writeJSON(w, http.StatusOK, out)
}

// handleRevive 手动把一个冷却中的账号放出来。
//
// 为什么要手动：自动冷却到期会复活，但如果用户已经修好了问题
// （比如换了令牌），没必要干等那 90 秒。
func (a *Admin) handleRevive(w http.ResponseWriter, r *http.Request) {
	if !a.requireAdmin(w, r) {
		return
	}
	provName := r.URL.Query().Get("provider")
	id := r.URL.Query().Get("id")
	for _, p := range a.router.Providers() {
		if p.Name() != provName {
			continue
		}
		pr, ok := p.(provider.PoolReporter)
		if !ok {
			writeJSON(w, http.StatusBadRequest, errObj("该上游不支持账号池操作"))
			return
		}
		// ?stats=1 走清统计，否则是复活账号。合成一个端点是因为
		// 两者都作用于「单个账号」，拆开只是多一个 URL。
		if r.URL.Query().Get("stats") == "1" {
			if !pr.ResetAccountStats(id) {
				writeJSON(w, http.StatusNotFound, errObj("账号不存在"))
				return
			}
			writeJSON(w, http.StatusOK, map[string]any{"ok": true})
			return
		}
		if !pr.ReviveAccount(id) {
			writeJSON(w, http.StatusNotFound, errObj("账号不存在"))
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})
		return
	}
	writeJSON(w, http.StatusNotFound, errObj("没有这个上游："+provName))
}

// syncProviders 让所有上游按最新配置重建账号池。
//
// 每次账号增删改后都要调用。之前只在 Copilot 的授权流程里调了 Sync，
// 手动添加账号的路径没调 —— 结果是 Copilot（走授权）显示 2/2 正常，
// 而手动加的 Agnes 账号不进池，控制台只显示 1 个。
//
// 全部同步而不是只同步受影响的那个：账号可能被改了 provider 归属，
// 逐个判断反而容易漏。
func (a *Admin) syncProviders() {
	for _, p := range a.router.Providers() {
		p.Sync()
	}
}

// ---------- 小工具 ----------

// writeJSON 输出 JSON 响应。
func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func errObj(msg string) map[string]any {
	return map[string]any{"error": msg}
}

// decodeBody 解析 JSON 请求体，并限制大小防止内存被打满。
func decodeBody(r *http.Request, v any) error {
	defer r.Body.Close()
	dec := json.NewDecoder(http.MaxBytesReader(nil, r.Body, 1<<20))
	if err := dec.Decode(v); err != nil {
		if errors.Is(err, io.EOF) {
			return nil // 空 body 是允许的（例如创建密钥时不填名字）
		}
		return fmt.Errorf("请求体不是合法 JSON：%w", err)
	}
	return nil
}
