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
	r := api.NewRouter(sub)

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
