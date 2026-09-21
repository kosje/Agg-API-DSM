// Command agg-api 是 Agg-API-DSM 的服务入口。
//
// 一个二进制、一个端口，同时提供两个上游的能力：
//   - Agnes AI   （多账号池化 + RPM 限流）
//   - M365 Copilot（WebSocket 长连接）
//
// 下游只认一个地址、一个 API Key，由模型名决定走哪个上游。
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"aggapi/internal/config"
	"aggapi/internal/gateway"
	"aggapi/internal/provider"
	"aggapi/internal/provider/agnes"
	"aggapi/internal/provider/copilot"
	"aggapi/internal/web"
)

// 版本号单一来源：发版脚本读这一行。
var version = "0.1.0"

// 默认端口。刻意避开两个上游各自占用的 4141（M365）与 4142（Agnes）：
// 合并后只装这一个套件，但如果用户机器上还留着旧的单上游套件，
// 端口不重叠才能并存排查。
const defaultPort = 4444

func main() {
	host := flag.String("host", env("AGG_HOST", "127.0.0.1"), "监听地址（0.0.0.0 表示允许局域网访问）")
	port := flag.String("port", env("AGG_PORT", fmt.Sprint(defaultPort)), "监听端口")
	dataDir := flag.String("data", env("AGG_DATA", ""), "数据目录（默认 <可执行文件目录>/data）")
	showVersion := flag.Bool("version", false, "打印版本后退出")
	flag.Parse()

	if *showVersion {
		fmt.Println("agg-api", version)
		return
	}

	if *dataDir == "" {
		if exe, err := os.Executable(); err == nil {
			*dataDir = filepath.Join(filepath.Dir(exe), "data")
		} else {
			*dataDir = "data"
		}
	}
	if abs, err := filepath.Abs(*dataDir); err == nil {
		*dataDir = abs
	}
	if err := os.MkdirAll(*dataDir, 0o755); err != nil {
		log.Fatalf("创建数据目录失败：%v", err)
	}

	store, err := config.NewStore(*dataDir)
	if err != nil {
		log.Fatalf("初始化数据目录失败：%v", err)
	}

	router := gateway.NewRouter()

	// 装配上游。新增上游只需在这里加一行 —— 网关核心不需要任何改动。
	for _, p := range []provider.Provider{
		agnes.New(store),
		copilot.New(store),
	} {
		if err := router.Register(p); err != nil {
			log.Fatalf("注册上游失败：%v", err)
		}
	}

	mux := http.NewServeMux()

	// 下游密钥校验。
	//
	// 一条密钥都没配时放行本机访问（方便首次部署后立刻能打开控制台），
	// 一旦配了就必须带密钥 —— 否则用户配了密钥却发现根本没生效，
	// 那种「以为已经锁上其实没锁」的状态比完全开放更危险。
	authorize := func(w http.ResponseWriter, r *http.Request) bool {
		if !store.HasKeys() {
			if isLoopback(r.RemoteAddr) {
				return true
			}
			writeJSON(w, http.StatusUnauthorized, map[string]any{"error": map[string]any{
				"message": "尚未配置任何下游 API Key，且请求来自非本机地址。" +
					"请先在控制台创建密钥。",
				"type": "no_key_configured",
			}})
			return false
		}
		token := bearer(r)
		if token == "" {
			writeJSON(w, http.StatusUnauthorized, map[string]any{"error": map[string]any{
				"message": "缺少 Authorization: Bearer <key>",
				"type":    "invalid_api_key",
			}})
			return false
		}
		id, ok := store.VerifyKey(token)
		if !ok {
			writeJSON(w, http.StatusUnauthorized, map[string]any{"error": map[string]any{
				"message": "API Key 无效或已停用",
				"type":    "invalid_api_key",
			}})
			return false
		}
		store.TouchKey(id)
		return true
	}

	gateway.NewHandler(router, authorize).Register(mux)

	// 管理 API 与控制台。
	gateway.NewAdmin(store, router).Register(mux)
	mux.HandleFunc("/", web.Console())

	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{
			"status":    "ok",
			"version":   version,
			"upstreams": upstreamHealth(router),
		})
	})

	addr := net.JoinHostPort(*host, *port)
	srv := &http.Server{
		Addr:              addr,
		Handler:           mux,
		ReadHeaderTimeout: 15 * time.Second,
	}

	ctx, stop := signal.NotifyContext(context.Background(),
		os.Interrupt, syscall.SIGTERM)
	defer stop()

	log.Printf("agg-api %s 已启动  监听 %s  数据目录 %s", version, addr, *dataDir)
	log.Printf("上游：%s", upstreamSummary(router))

	go func() {
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Fatalf("监听失败：%v", err)
		}
	}()

	<-ctx.Done()
	log.Println("收到停止信号，正在优雅关闭…")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		log.Printf("关闭超时：%v", err)
	}
	log.Println("agg-api 已停止。")
}

func env(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

// bearer 从 Authorization 头取出 Bearer 令牌。
func bearer(r *http.Request) string {
	v := strings.TrimSpace(r.Header.Get("Authorization"))
	if len(v) > 7 && strings.EqualFold(v[:7], "bearer ") {
		return strings.TrimSpace(v[7:])
	}
	return ""
}

// isLoopback 判断请求是否来自本机。
func isLoopback(remoteAddr string) bool {
	host, _, err := net.SplitHostPort(remoteAddr)
	if err != nil {
		host = remoteAddr
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

// upstreamHealth 汇总各上游的健康状况。
// 注意：/v1/models 不在这里实现 —— 它属于网关的 OpenAI 兼容端点，
// 与路由、鉴权放在一起，见 internal/gateway/openai.go。
func upstreamHealth(r *gateway.Router) map[string]any {
	out := map[string]any{}
	for _, p := range r.Providers() {
		h := p.Health()
		out[p.Name()] = map[string]any{"ready": h.Ready, "detail": h.Detail}
	}
	return out
}

func upstreamSummary(r *gateway.Router) string {
	s := ""
	for _, p := range r.Providers() {
		if s != "" {
			s += "；"
		}
		s += fmt.Sprintf("%s(%s) %d 个模型",
			p.DisplayName(), p.Name(), len(p.Models()))
	}
	return s
}
