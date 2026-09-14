// Package api 组装 HTTP 路由。
package api

import (
	"io"
	"io/fs"
	"net/http"
	"path"
	"strings"

	"github.com/go-chi/chi/v5"
)

// NewRouter 返回面板的根路由：API 在后续任务中挂载，
// 其余路径交给嵌入的前端产物，未知路径回退到 index.html（SPA 前端路由）。
func NewRouter(static fs.FS) chi.Router {
	r := chi.NewRouter()
	fileServer := http.FileServerFS(static)
	r.Handle("/*", spaHandler(static, fileServer))
	return r
}

// spaHandler 命中不到真实文件时返回 index.html，让前端路由接管。
func spaHandler(static fs.FS, fileServer http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		p := strings.TrimPrefix(path.Clean("/"+req.URL.Path), "/")
		if p != "" && p != "index.html" {
			if f, err := static.Open(p); err == nil {
				_ = f.Close()
				fileServer.ServeHTTP(w, req)
				return
			}
		}
		index, err := fs.ReadFile(static, "index.html")
		if err != nil {
			http.NotFound(w, req)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = io.WriteString(w, string(index))
	})
}
