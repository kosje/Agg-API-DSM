// Package gateway 是网关核心：对外提供 OpenAI 兼容端点，对内按模型名路由到上游。
//
// 它刻意不认识任何具体上游 —— 只持有 provider.Provider 列表。
package gateway

import (
	"fmt"
	"sort"
	"strings"
	"sync"

	"aggapi/internal/provider"
)

// Router 维护「模型名 -> 上游」的映射。
//
// 模型名全局唯一，因此不需要按前缀猜：注册时建立索引，请求时一次查表。
// 这样新增上游不会与既有模型名冲突（冲突会在注册时直接报错，而不是运行时走错上游）。
type Router struct {
	mu        sync.RWMutex
	providers []provider.Provider
	byModel   map[string]provider.Provider
	models    []provider.Model
}

func NewRouter() *Router {
	return &Router{byModel: map[string]provider.Provider{}}
}

// Register 注册一个上游，并把它声明的模型并入索引。
//
// 模型名重复会直接返回错误：宁可启动失败，也不要在运行时把请求发错上游。
func (r *Router) Register(p provider.Provider) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	name := p.Name()
	for _, existing := range r.providers {
		if existing.Name() == name {
			return fmt.Errorf("上游 %q 重复注册", name)
		}
	}

	for _, m := range p.Models() {
		if m.ID == "" {
			return fmt.Errorf("上游 %q 声明了空模型名", name)
		}
		if owner, dup := r.byModel[m.ID]; dup {
			return fmt.Errorf("模型名 %q 冲突：%s 与 %s 都声明了它",
				m.ID, owner.Name(), name)
		}
		r.byModel[m.ID] = p
		r.models = append(r.models, m)
	}

	r.providers = append(r.providers, p)
	return nil
}

// Lookup 按模型名找上游。
func (r *Router) Lookup(model string) (provider.Provider, provider.Model, error) {
	r.mu.RLock()
	p, ok := r.byModel[model]
	var found provider.Model
	if ok {
		for _, m := range r.models {
			if m.ID == model {
				found = m
				break
			}
		}
	}
	r.mu.RUnlock()

	if !ok {
		// 注意：ModelIDs() 自己会取读锁，必须在释放之后再调，
		// 否则与并发的写操作形成死锁（Go 的 RWMutex 不可重入）。
		return nil, provider.Model{}, fmt.Errorf("未知模型 %q（可用：%s）",
			model, strings.Join(r.ModelIDs(), ", "))
	}
	if found.ID == "" {
		found = provider.Model{ID: model}
	}
	return p, found, nil
}

// Providers 返回已注册的上游，顺序与注册顺序一致。
func (r *Router) Providers() []provider.Provider {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]provider.Provider, len(r.providers))
	copy(out, r.providers)
	return out
}

// Models 返回全部模型，按「上游注册顺序 + 模型名」稳定排序，
// 保证 /v1/models 的输出可复现（客户端与测试都依赖这一点）。
func (r *Router) Models() []provider.Model {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]provider.Model, len(r.models))
	copy(out, r.models)
	return out
}

// ModelIDs 返回全部模型名。
func (r *Router) ModelIDs() []string {
	models := r.Models()
	ids := make([]string, 0, len(models))
	for _, m := range models {
		ids = append(ids, m.ID)
	}
	sort.Strings(ids)
	return ids
}

// Supports 判断某模型是否具备给定能力。
//
// 网关在转发前先查一次，不支持就直接返回明确错误 ——
// 比把请求发出去、拿回一个语焉不详的上游报错要好。
func (r *Router) Supports(model string, want provider.Capability) bool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	for _, m := range r.models {
		if m.ID == model {
			return m.Caps.Has(want)
		}
	}
	return false
}

// PickImageModel 挑一个声明了生图能力的模型。
//
// 传空 model 时自动挑一个（优先 copilot-auto，它是上游的智能路由）；
// 传了名字就精确匹配，并要求它确实声明了生图能力 ——
// 用纯文本模型生图只会拿到一段「我不能画图」的文字回复。
func (r *Router) PickImageModel(name string) (provider.Model, provider.Provider, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()

	find := func(id string) (provider.Model, bool) {
		for _, m := range r.models {
			if m.ID == id {
				return m, true
			}
		}
		return provider.Model{}, false
	}
	resolve := func(m provider.Model) (provider.Model, provider.Provider, error) {
		up, ok := r.byModel[m.ID]
		if !ok {
			return provider.Model{}, nil, fmt.Errorf("%w：模型 %s 没有归属的上游",
				provider.ErrUnsupported, m.ID)
		}
		return m, up, nil
	}

	if name != "" {
		m, ok := find(name)
		if !ok {
			return provider.Model{}, nil, fmt.Errorf("%w：没有模型 %s", provider.ErrUnsupported, name)
		}
		if !m.Caps.Has(provider.CapImage) {
			return provider.Model{}, nil, fmt.Errorf(
				"%w：模型 %s 不支持生图，请用 gpt-image-2 或 copilot-auto",
				provider.ErrUnsupported, name)
		}
		return resolve(m)
	}

	// 没指定：优先 copilot-auto（上游智能路由，会自己决定画不画）
	if m, ok := find("copilot-auto"); ok && m.Caps.Has(provider.CapImage) {
		return resolve(m)
	}
	// 其次找「纯生图」模型（只有 CapImage、不带 CapText）
	for _, m := range r.models {
		if m.Caps.Has(provider.CapImage) && !m.Caps.Has(provider.CapText) {
			return resolve(m)
		}
	}
	for _, m := range r.models {
		if m.Caps.Has(provider.CapImage) {
			return resolve(m)
		}
	}
	return provider.Model{}, nil, fmt.Errorf("%w：没有可用的生图模型", provider.ErrUnsupported)
}
