// Package gateway 的图片中转。
//
// 为什么必须中转，不能把上游 URL 直接给客户端：
//
//	Copilot 生成的图片放在需要**微软令牌鉴权**的临时地址上。
//	客户端拿到那个 URL 去下载只会得到 401 —— 用户看到的是「图生成了但显示不出来」。
//
// 所以网关要自己带令牌把图下下来，存一份，再给客户端一个**本网关的 URL**。
// 这是 M365-Copilot2API 的做法（generatedImageURL / generatedImageFile），
// 也是唯一能让客户端真正拿到图的路径。
package gateway

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"net/http"
	"strings"
	"sync"
	"time"
)

// imageTTL 是缓存里图片的存活时间。
//
// 30 分钟：足够客户端下载（通常是立刻），又不至于让 NAS 内存长期占着。
// 上游的临时 URL 本身也有有效期，存太久反而会拿到过期的。
const imageTTL = 30 * time.Minute

// imageCacheMax 是缓存条目上限。
//
// 按每张图平均 1MB 估算，256 条最多占 256MB —— NAS 内存通常不宽裕，
// 这个上限要在「够客户端下载」与「不把内存吃光」之间取平衡。
const imageCacheMax = 256

// cachedImage 是缓存里的一张图。
type cachedImage struct {
	data     []byte
	mime     string
	at       time.Time
}

// imageStore 是按 id 存取生成图片的短期缓存。
//
// 刻意只放内存：这些是「刚生成、等着客户端来取」的临时图，
// 落盘会带来清理、权限、磁盘占用一堆问题，而重启后丢掉的代价很小
// （用户重发一次就好）。
type imageStore struct {
	mu    sync.Mutex
	items map[string]cachedImage
}

func newImageStore() *imageStore {
	return &imageStore{items: map[string]cachedImage{}}
}

// put 存入一张图，返回它的 id。
func (s *imageStore) put(data []byte, mime string) string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		// 拿不到随机数就退回时间戳 —— 概率极低，但不能让生图整个失败。
		return hex.EncodeToString([]byte(time.Now().Format(time.RFC3339Nano)))
	}
	id := hex.EncodeToString(b[:])

	s.mu.Lock()
	defer s.mu.Unlock()
	s.evictLocked()
	s.items[id] = cachedImage{data: data, mime: mime, at: time.Now()}
	return id
}

// get 取出图片。
func (s *imageStore) get(id string) ([]byte, string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	it, ok := s.items[id]
	if !ok {
		return nil, "", false
	}
	if time.Since(it.at) > imageTTL {
		delete(s.items, id)
		return nil, "", false
	}
	return it.data, it.mime, true
}

// evictLocked 清掉过期项；仍超上限时按时间淘汰最旧的。
//
// 两步走的原因：过期清理不保证数量降下来（比如短时间内涌进几百张），
// 所以还要有个硬上限兜底。
func (s *imageStore) evictLocked() {
	now := time.Now()
	for id, it := range s.items {
		if now.Sub(it.at) > imageTTL {
			delete(s.items, id)
		}
	}
	for len(s.items) >= imageCacheMax {
		oldestID, oldest := "", now
		for id, it := range s.items {
			if it.at.Before(oldest) || oldestID == "" {
				oldestID, oldest = id, it.at
			}
		}
		if oldestID == "" {
			break
		}
		delete(s.items, oldestID)
	}
}

// dataURL 把图片编码成 data URL，供内联到 markdown 里。
//
// MIME 缺失时靠内容嗅探补上 —— 少了它浏览器不知道该按什么格式解码，
// 会直接不显示。
func dataURL(data []byte, mime string) string {
	if mime == "" || mime == "application/octet-stream" {
		mime = http.DetectContentType(data)
	}
	if i := strings.IndexByte(mime, ';'); i >= 0 {
		mime = mime[:i]
	}
	return "data:" + mime + ";base64," + base64.StdEncoding.EncodeToString(data)
}

// imageURL 拼出客户端可访问的图片地址。
//
// 必须尊重反代传来的 X-Forwarded-Proto / Host：
// 用户走 https://域名 访问时，返回 http://内网IP 的地址客户端是打不开的。
func imageURL(r *http.Request, id string) string {
	scheme := "http"
	if r.TLS != nil || strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https") {
		scheme = "https"
	}
	host := r.Host
	// 反代常把原始 Host 放在 X-Forwarded-Host，优先用它。
	if h := r.Header.Get("X-Forwarded-Host"); h != "" {
		host = h
	}
	return scheme + "://" + host + "/v1/images/files/" + id
}

// handleImageFile 把缓存的图片发给客户端。
//
// 这个端点**不校验 API Key**：图片 URL 里的 id 是 16 字节随机数，
// 猜不到；而要求客户端在 <img> 标签里带鉴权头是不现实的 ——
// 浏览器加载图片不会带自定义头。用不可猜测的 id 换取「能直接显示」，
// 是这里的正确取舍。
func (h *Handler) handleImageFile(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	id := strings.TrimPrefix(r.URL.Path, "/v1/images/files/")
	if id == "" {
		http.NotFound(w, r)
		return
	}
	data, mime, ok := h.images.get(id)
	if !ok {
		// 过期或不存在。给出明确原因 —— 客户端只看到 404 会以为是网关坏了。
		http.Error(w, "图片不存在或已过期（生成后 30 分钟内可下载）", http.StatusNotFound)
		return
	}
	if mime == "" {
		mime = http.DetectContentType(data)
	}
	w.Header().Set("Content-Type", mime)
	w.Header().Set("Content-Length", itoa(len(data)))
	// 内容不可变，让客户端与中间层放心缓存。
	w.Header().Set("Cache-Control", "private, max-age=1800")
	w.WriteHeader(http.StatusOK)
	if r.Method == http.MethodHead {
		return
	}
	_, _ = w.Write(data)
}

// itoa 避免为了一个数字引入 strconv。
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	return string(buf[i:])
}
