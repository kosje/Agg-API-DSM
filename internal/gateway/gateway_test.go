package gateway

import (
	"crypto/tls"
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"aggapi/internal/provider"
)

// TestImageStorePutGet 存取基本可用。
func TestImageStorePutGet(t *testing.T) {
	s := newImageStore()
	data := []byte("\x89PNG\r\n\x1a\nfake")
	id := s.put(data, "image/png")

	got, mime, ok := s.get(id)
	if !ok {
		t.Fatal("刚存进去的图取不到")
	}
	if string(got) != string(data) {
		t.Fatal("取回的内容与存入的不一致")
	}
	if mime != "image/png" {
		t.Fatalf("mime = %q", mime)
	}
	if _, _, ok := s.get("不存在"); ok {
		t.Fatal("不存在的 id 不该取到东西")
	}
}

// TestImageStoreEvictsOldest 超过上限时淘汰最旧的。
//
// 没有上限的话，连续生图会把 NAS 内存吃光。
func TestImageStoreEvictsOldest(t *testing.T) {
	s := newImageStore()
	// 塞到刚超过上限
	for i := 0; i < imageCacheMax+5; i++ {
		s.put([]byte{byte(i)}, "image/png")
	}
	s.mu.Lock()
	n := len(s.items)
	s.mu.Unlock()
	if n > imageCacheMax {
		t.Fatalf("缓存条目 %d，超过上限 %d", n, imageCacheMax)
	}
}

// TestImageStoreExpires 过期项不再可取。
func TestImageStoreExpires(t *testing.T) {
	s := newImageStore()
	id := s.put([]byte("x"), "image/png")

	// 手动把时间调旧，避免真的等 30 分钟。
	s.mu.Lock()
	it := s.items[id]
	it.at = time.Now().Add(-imageTTL - time.Minute)
	s.items[id] = it
	s.mu.Unlock()

	if _, _, ok := s.get(id); ok {
		t.Fatal("过期图片仍可取到")
	}
}

// TestDataURL 生成的 data URL 要能被浏览器直接解码。
//
// MIME 缺失时必须靠内容嗅探补上 —— 少了它浏览器不知道该按什么格式解码，
// 会直接不显示，而用户只会看到「图没出来」。
func TestDataURL(t *testing.T) {
	// 一段最小 PNG 头，让 DetectContentType 能认出来。
	png := []byte{0x89, 'P', 'N', 'G', 0x0d, 0x0a, 0x1a, 0x0a}

	got := dataURL(png, "")
	if !strings.HasPrefix(got, "data:image/png;base64,") {
		t.Fatalf("MIME 缺失时未嗅探出 png：%q", got[:40])
	}
	want := base64.StdEncoding.EncodeToString(png)
	if !strings.HasSuffix(got, want) {
		t.Fatal("base64 内容不对")
	}

	// 带 charset 的 MIME 要清掉参数，否则 data URL 不合法。
	got2 := dataURL(png, "image/png; charset=binary")
	if strings.Contains(got2, "charset") {
		t.Fatalf("MIME 参数没清掉：%q", got2[:40])
	}
}

// TestImageURLRespectsProxyHeaders 走反代时要给出正确的对外地址。
//
// 用户在 https://域名 访问时，返回 http://内网IP 的图片地址客户端打不开。
func TestImageURLRespectsProxyHeaders(t *testing.T) {
	cases := []struct {
		name   string
		header map[string]string
		tls    bool
		want   string
	}{
		{"直连 http", nil, false, "http://nas:4444/v1/images/files/abc"},
		{"直连 https", nil, true, "https://nas:4444/v1/images/files/abc"},
		{"反代声明 https", map[string]string{"X-Forwarded-Proto": "https"},
			false, "https://nas:4444/v1/images/files/abc"},
		{"反代改 Host", map[string]string{
			"X-Forwarded-Proto": "https", "X-Forwarded-Host": "agg.example.com"},
			false, "https://agg.example.com/v1/images/files/abc"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodGet, "http://nas:4444/", nil)
			for k, v := range c.header {
				r.Header.Set(k, v)
			}
			if c.tls {
				r.TLS = &tls.ConnectionState{}
			}
			if got := imageURL(r, "abc"); got != c.want {
				t.Fatalf("imageURL = %q，期望 %q", got, c.want)
			}
		})
	}
}

// TestHandleImageFile 图片端点要能正确返回内容。
func TestHandleImageFile(t *testing.T) {
	h := &Handler{images: newImageStore()}
	png := []byte{0x89, 'P', 'N', 'G', 0x0d, 0x0a, 0x1a, 0x0a, 1, 2, 3}
	id := h.images.put(png, "image/png")

	rec := httptest.NewRecorder()
	h.handleImageFile(rec, httptest.NewRequest(http.MethodGet, "/v1/images/files/"+id, nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("状态码 %d", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "image/png" {
		t.Fatalf("Content-Type = %q", ct)
	}
	if rec.Body.Len() != len(png) {
		t.Fatalf("返回 %d 字节，期望 %d", rec.Body.Len(), len(png))
	}

	// 不存在的 id 要给明确原因，而不是笼统 404 ——
	// 客户端只看到 404 会以为网关坏了。
	rec2 := httptest.NewRecorder()
	h.handleImageFile(rec2, httptest.NewRequest(http.MethodGet, "/v1/images/files/nope", nil))
	if rec2.Code != http.StatusNotFound {
		t.Fatalf("不存在的图应返回 404，实际 %d", rec2.Code)
	}
	if !strings.Contains(rec2.Body.String(), "过期") {
		t.Fatalf("错误信息没说明原因：%q", rec2.Body.String())
	}
}

// TestParseAttachmentsCSV 附件文本要拼进用户消息，且不顶掉原提问。
func TestParseAttachmentsCSV(t *testing.T) {
	csv := "城市,人口\n北京,2189万\n"
	raw := map[string]any{
		"attachments": []any{
			map[string]any{
				"name": "cities.csv", "mime": "text/csv",
				"data": base64.StdEncoding.EncodeToString([]byte(csv)),
			},
		},
	}
	images, docText, err := parseAttachments(raw)
	if err != nil {
		t.Fatalf("解析失败：%v", err)
	}
	if len(images) != 0 {
		t.Fatalf("不该有图片，实际 %d 张", len(images))
	}
	if !strings.Contains(docText, "北京") {
		t.Fatalf("附件内容没提取出来：%q", docText)
	}
	if !strings.Contains(docText, "cities.csv") {
		t.Fatal("附件名没带上 —— 用户不知道这是哪份文件的内容")
	}
}

// TestParseAttachmentsRejectsUnknownType 不支持的类型要明确拒绝。
//
// 猜着解析会把二进制当文本塞给模型，输出莫名其妙 ——
// 比直接说「不支持」更难排查。
func TestParseAttachmentsRejectsUnknownType(t *testing.T) {
	raw := map[string]any{
		"attachments": []any{
			map[string]any{
				"name": "x.exe", "mime": "application/octet-stream",
				"data": base64.StdEncoding.EncodeToString([]byte{0, 1, 2}),
			},
		},
	}
	if _, _, err := parseAttachments(raw); err == nil {
		t.Fatal("不支持的附件类型应报错")
	}
}

// TestParseAttachmentsBadBase64 坏的 base64 要报错而不是静默跳过。
func TestParseAttachmentsBadBase64(t *testing.T) {
	raw := map[string]any{
		"attachments": []any{
			map[string]any{"name": "a.csv", "mime": "text/csv", "data": "!!!不是 base64!!!"},
		},
	}
	if _, _, err := parseAttachments(raw); err == nil {
		t.Fatal("坏 base64 应报错")
	}
}

// TestCollectDataURLs 只收 data URL，不收 http 链接。
//
// 收 http 链接等于让上游去访问用户给的地址，那是 SSRF 面。
func TestCollectDataURLs(t *testing.T) {
	raw := map[string]any{
		"messages": []any{
			map[string]any{
				"role": "user",
				"content": []any{
					map[string]any{"type": "text", "text": "看图"},
					map[string]any{"image_url": map[string]any{"url": "data:image/png;base64,AAAA"}},
					map[string]any{"image_url": "https://evil.example.com/x.png"},
				},
			},
		},
	}
	got := collectDataURLs(raw)
	if len(got) != 1 {
		t.Fatalf("收了 %d 个，期望只收 1 个 data URL：%v", len(got), got)
	}
	if !strings.HasPrefix(got[0], "data:") {
		t.Fatalf("收进来一个非 data URL：%q", got[0])
	}
}

// TestRetriable 重试判定。
//
// 这个判断错了代价很大：把不该重试的（凭据失效）重试一遍，
// 会白烧配额、拖长响应；把该重试的（没配额）判成不重试，
// 账号轮询就形同虚设。
func TestRetriable(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"没配额 -> 换账号", provider.ErrNoCapacity, true},
		{"上游错误 -> 换账号", provider.ErrUpstream, true},
		{"凭据失效 -> 不重试", provider.ErrAuth, false},
		{"能力不支持 -> 不重试", provider.ErrUnsupported, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := retriable(c.err); got != c.want {
				t.Fatalf("retriable = %v，期望 %v", got, c.want)
			}
		})
	}
}

// TestBackoffGrows 退避要递增，但不能大到让用户白等。
func TestBackoffGrows(t *testing.T) {
	a, b := backoff(0), backoff(1)
	if b <= a {
		t.Fatalf("退避没有递增：%v -> %v", a, b)
	}
	if b > 2*time.Second {
		t.Fatalf("单次退避 %v 过长", b)
	}
}
