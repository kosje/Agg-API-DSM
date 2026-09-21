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
	"syscall"
	"time"

	"aggapi/internal/config"
	"aggapi/internal/gateway"
	"aggapi/internal/provider"
	"aggapi/internal/provider/agnes"
	"aggapi/internal/provider/copilot"
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
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{
			"status":    "ok",
			"version":   version,
			"upstreams": upstreamHealth(router),
		})
	})
	mux.HandleFunc("/v1/models", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{
			"object": "list",
			"data":   modelList(router),
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

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

// modelList 把各上游的模型汇总成 OpenAI 的 /v1/models 形状。
// owned_by 填上游名，客户端据此就能看出某个模型实际走哪条链路。
func modelList(r *gateway.Router) []map[string]any {
	owner := map[string]string{}
	for _, p := range r.Providers() {
		for _, m := range p.Models() {
			owner[m.ID] = p.Name()
		}
	}
	models := r.Models()
	out := make([]map[string]any, 0, len(models))
	for _, m := range models {
		out = append(out, map[string]any{
			"id":       m.ID,
			"object":   "model",
			"owned_by": owner[m.ID],
			"created":  0,
		})
	}
	return out
}

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
