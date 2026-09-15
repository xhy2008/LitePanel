package api_test

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"litepanel/internal/api"
	"litepanel/internal/webdist"
)

// M1-T1 验收：GET / 返回 200，且响应体包含 Vite 入口的挂载点。
func TestEmbedServesIndexHTML(t *testing.T) {
	sub, err := webdist.Dist()
	if err != nil {
		t.Fatalf("webdist.Dist: %v", err)
	}
	r := api.NewRouter(sub, api.AuthDeps{})

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, `<div id="app">`) {
		t.Fatalf("响应体应包含 <div id=\"app\">，实际：%s", body)
	}
}

// SPA 兜底不得把「资源缺失」伪装成「成功」。
// 真实事故场景：重新构建后 hash 变了，浏览器缓存里旧 index.html 仍引用
// 旧 hash 的 JS；兜底返回 200 + HTML，浏览器因 MIME 不符拒绝当模块执行
// —— 结果是白屏且不抛异常，前端任何错误捕获都看不到。
func TestMissingAssetReturns404NotIndexHTML(t *testing.T) {
	sub, err := webdist.Dist()
	if err != nil {
		t.Fatalf("webdist.Dist: %v", err)
	}
	r := api.NewRouter(sub, api.AuthDeps{})

	req := httptest.NewRequest(http.MethodGet, "/assets/index-deadbeef.js", nil)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("缺失的静态资源应 404, got %d (body=%s)", rec.Code, rec.Body.String())
	}
}

// 但前端路由（/term、/settings…）必须继续回 index.html + 200，
// 否则刷新子页面就变成 404。
func TestFrontendRouteStillFallsBackToIndex(t *testing.T) {
	sub, err := webdist.Dist()
	if err != nil {
		t.Fatalf("webdist.Dist: %v", err)
	}
	r := api.NewRouter(sub, api.AuthDeps{})

	for _, p := range []string{"/", "/term", "/files", "/settings"} {
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, p, nil))
		if rec.Code != http.StatusOK {
			t.Errorf("%s 应 200, got %d", p, rec.Code)
		}
		if !strings.Contains(rec.Body.String(), `<div id="app">`) {
			t.Errorf("%s 应返回 index.html", p)
		}
	}
}

// 判断依据是「像不像静态资源」，不是目录名：
// 带扩展名的路径一律不回 index.html，避免同类问题换目录复现。
func TestExtensionBearingUnknownPathIs404(t *testing.T) {
	sub, err := webdist.Dist()
	if err != nil {
		t.Fatalf("webdist.Dist: %v", err)
	}
	r := api.NewRouter(sub, api.AuthDeps{})

	for _, p := range []string{"/favicon.ico", "/missing.css", "/nested/deep.js"} {
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, p, nil))
		if rec.Code != http.StatusNotFound {
			t.Errorf("%s 应 404, got %d", p, rec.Code)
		}
	}
}
