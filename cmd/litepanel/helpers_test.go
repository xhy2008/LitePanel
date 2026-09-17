package main

import (
	"io"
	"net/http/httptest"
	"testing"

	"litepanel/internal/api"
	"litepanel/internal/auth"
	"litepanel/internal/store"
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

// seedPasswordNoChange 直接写入一个"已完成改密"的账号。
//
// 走 auth.SetPassword（而不是 SetInitialPassword）是关键：前者不写
// must_change_password 标志，缺该标志时 MustChangePassword 返回 false。
// 用 SetInitialPassword 会让所有请求被强制改密中间件挡成 403。
// 刻意复用生产函数而不是手写 SQL：表结构或标志位语义一变，这里立刻红。
func seedPasswordNoChange(db *store.DB, plain string) error {
	hash, err := auth.HashPassword(plain)
	if err != nil {
		return err
	}
	return auth.SetPassword(db, hash)
}
