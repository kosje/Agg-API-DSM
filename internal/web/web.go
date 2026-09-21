// Package web 内嵌控制台单页。
//
// 用 go:embed 把 HTML 打进二进制：群晖上只有一个可执行文件，
// 不必再管静态目录的权限与路径问题。
package web

import (
	_ "embed"
	"net/http"
)

//go:embed console.html
var consoleHTML []byte

// Console 返回控制台页面的处理函数。
//
// 只对根路径与 /console 生效，其余交给 404 ——
// 否则会把 /v1/... 之类的接口请求也吞掉。
func Console() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" && r.URL.Path != "/console" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		// 控制台是动态页面且含鉴权逻辑，不让中间层缓存。
		w.Header().Set("Cache-Control", "no-store")
		_, _ = w.Write(consoleHTML)
	}
}
