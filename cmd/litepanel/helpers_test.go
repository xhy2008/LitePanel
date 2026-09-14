package main

import (
	"io"
	"net/http/httptest"
	"testing"

	"litepanel/internal/api"
)

// newTestServer 用与真实入口相同的路由装配起一个测试服务（不挂前端静态资源）。
func newTestServer(t *testing.T, deps api.AuthDeps) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(api.NewRouter(nil, deps))
	t.Cleanup(srv.Close)
	return srv
}

func get(t *testing.T, c *httptest.Server, path string) {
	t.Helper()
	resp, err := c.Client().Get(c.URL + path)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	_, _ = io.ReadAll(resp.Body)
}
