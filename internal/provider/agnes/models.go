// Package agnes 的模型目录。
//
// 与 Copilot 侧不同，Agnes 是标准的 OpenAI 兼容服务，上游自己就提供
// `/v1/models`。所以这里分两层：
//
//  1. **静态目录**：agnes 官方公布过的模型名，作为兜底与能力标注依据
//  2. **实时清单**：启动后异步查一次上游的 `/v1/models`，拿到什么用什么
//
// 为什么两层都要：只靠静态目录，上游上了新模型这边看不到；
// 只靠实时查询，网络不通或 Key 没配时控制台就一片空白，用户会以为坏了。
package agnes

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"aggapi/internal/provider"
)

// 静态目录。取自 agnes 官方模型列表，与上游项目的 pool.TextModels 等保持一致。
var (
	textModels = []string{
		"agnes-3.0-flash",
		"agnes-2.5-pro",
		"agnes-2.5-pro-alpha",
		"agnes-2.5-pro-beta",
		"agnes-2.5-flash",
		"agnes-2.0-flash",
		"agnes-1.5-flash",
	}
	imageModels = []string{
		"agnes-image-2.5-flash",
		"agnes-image-2.1-flash",
		"agnes-image-2.0-flash",
	}
	videoModels = []string{
		"agnes-video-2.5",
		"agnes-video-2.5-flash",
		"agnes-video-v2.0",
	}
)

// staticModels 把静态目录转成 provider.Model。
func staticModels() []provider.Model {
	out := make([]provider.Model, 0, len(textModels)+len(imageModels)+len(videoModels))
	for _, id := range textModels {
		out = append(out, provider.Model{
			ID: id, Upstream: id, Desc: "文本",
			Caps: provider.CapText | provider.CapStream | provider.CapVision |
				provider.CapDoc,
		})
	}
	for _, id := range imageModels {
		out = append(out, provider.Model{
			ID: id, Upstream: id, Desc: "图片生成",
			Caps: provider.CapImage,
		})
	}
	for _, id := range videoModels {
		out = append(out, provider.Model{
			ID: id, Upstream: id, Desc: "视频生成",
			Caps: provider.CapVideo,
		})
	}
	return out
}

// capsFor 按模型名推断能力。
//
// 上游的 /v1/models 只给名字，不给能力。名字里带 image / video 的按图片、
// 视频处理，其余按文本 —— 这个推断在 agnes 的命名规范下是可靠的。
func capsFor(id string) provider.Capability {
	low := strings.ToLower(id)
	switch {
	case strings.Contains(low, "video"):
		return provider.CapVideo
	case strings.Contains(low, "image"), strings.Contains(low, "dall-e"):
		return provider.CapImage
	default:
		return provider.CapText | provider.CapStream | provider.CapVision |
			provider.CapDoc
	}
}

// discovered 缓存从上游拉到的模型清单。
type discovered struct {
	mu     sync.RWMutex
	ids    []string
	at     time.Time
	failed string
}

var live discovered

// refreshModels 异步查一次上游的 /v1/models。
//
// 失败不报错只记下来：控制台会退回静态目录，用户至少能看到东西。
func (p *Provider) refreshModels() {
	accounts := p.store.AccountsFor(p.Name())
	if len(accounts) == 0 {
		return
	}
	acct := accounts[0]

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		upstreamURL(acct, "/v1/models"), nil)
	if err != nil {
		return
	}
	req.Header.Set("Authorization", "Bearer "+acct.APIKey)
	req.Header.Set("User-Agent", userAgent)

	resp, err := p.client.Do(req)
	if err != nil {
		live.setFailure(err.Error())
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		live.setFailure("上游返回 HTTP " + resp.Status)
		return
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		live.setFailure(err.Error())
		return
	}

	var payload struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.Unmarshal(raw, &payload); err != nil {
		live.setFailure("响应不是合法的模型列表")
		return
	}

	ids := make([]string, 0, len(payload.Data))
	seen := map[string]bool{}
	for _, m := range payload.Data {
		id := strings.TrimSpace(m.ID)
		if id == "" || seen[id] {
			continue
		}
		seen[id] = true
		ids = append(ids, id)
	}
	if len(ids) == 0 {
		live.setFailure("上游返回了空列表")
		return
	}
	sort.Strings(ids)
	live.set(ids)
}

func (d *discovered) set(ids []string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.ids = ids
	d.at = time.Now()
	d.failed = ""
}

func (d *discovered) setFailure(msg string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.failed = msg
}

func (d *discovered) snapshot() ([]string, time.Time, string) {
	d.mu.RLock()
	defer d.mu.RUnlock()
	out := make([]string, len(d.ids))
	copy(out, d.ids)
	return out, d.at, d.failed
}

// Models 返回 Agnes 侧对外暴露的模型。
//
// 优先用实时清单（那是上游真实支持的），拉不到时退回静态目录。
// 同时把**账号里配置的模型**也带上 —— 用户可能手动指定了上游没列的模型，
// 那说明他知道自己在做什么，不该被过滤掉。
func (p *Provider) Models() []provider.Model {
	seen := map[string]bool{}
	out := []provider.Model{}

	// agnes-auto 是聚合入口：不指定具体模型时由网关挑一个可用的。
	// 它必须始终在列表里 —— 用户与客户端都习惯先选「auto」，
	// 而且它是唯一一个「换了账号也不用改客户端配置」的稳定名字。
	out = append(out, provider.Model{
		ID: "agnes-auto", Upstream: "agnes-auto", Desc: "自动选一个可用模型",
		Caps: provider.CapText | provider.CapStream | provider.CapVision | provider.CapDoc,
	})
	seen["agnes-auto"] = true

	add := func(id, desc string) {
		if id == "" || seen[id] {
			return
		}
		seen[id] = true
		out = append(out, provider.Model{
			ID: id, Upstream: id, Desc: desc, Caps: capsFor(id),
		})
	}

	// 账号里配置的模型优先（用户显式指定过）
	for _, a := range p.store.AccountsFor(p.Name()) {
		for _, m := range a.Models {
			add(m, "账号配置")
		}
	}

	if ids, _, _ := live.snapshot(); len(ids) > 0 {
		for _, id := range ids {
			add(id, "上游提供")
		}
		return out
	}

	// 拉不到上游清单：退回静态目录，并补一个聚合别名
	for _, m := range staticModels() {
		add(m.ID, m.Desc)
	}
	return out
}

// resolveAuto 把 agnes-auto 解析成账号里真实存在的模型。
//
// 上游不认识 agnes-auto 这个名字，直接发过去会被拒。所以在这里替换成
// 账号配置的第一个模型（用户显式声明过的，最可信）；账号没声明就退回
// 从上游拉到的清单；都没有则原样返回，让上游给出明确错误。
func (p *Provider) resolveAuto(model string) string {
	if model != "agnes-auto" {
		return model
	}
	for _, a := range p.store.AccountsFor(p.Name()) {
		if len(a.Models) > 0 {
			return a.Models[0]
		}
	}
	if ids, _, _ := live.snapshot(); len(ids) > 0 {
		return ids[0]
	}
	return model
}

// ModelSource 返回模型清单的来源说明，供控制台显示。
//
// 为什么要暴露这个：用户看到 agnes-auto 这种占位名会以为配错了，
// 而实际原因可能是「账号没配」或「拉不到上游清单」。把原因写出来，
// 就不用猜。
func (p *Provider) ModelSource() string {
	ids, at, failed := live.snapshot()
	if len(ids) > 0 {
		return "上游实时清单（" + at.Format("15:04") + " 拉取）"
	}
	if failed != "" {
		return "上游清单拉取失败（" + failed + "），显示内置目录"
	}
	return "内置目录"
}
