//go:build !debug

package api_test

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"testing"
)

// 发布构建的核心承诺（D9/§12.1）：就算 -debug 一路传进来，
// 访问日志中间件也在**编译期**就不存在了 —— 不是运行时开/关，
// 变参求值与字符串拼接都不会发生。
//
// 这条测试在 release 下必须绿；如果有人把挂载点从
// `if logx.Enabled && ...` 改回纯运行时判断 `if deps.Debug`，它会变红。
func TestAccessLogCompiledOutEvenWhenDebugFlagSet(t *testing.T) {
	var log bytes.Buffer
	srv := httptest.NewServer(newDebugRouter(t, &log, true)) // debug=true 传进去
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/api/me")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()

	if log.Len() != 0 {
		t.Fatalf("发布构建即便 -debug=true 也不得有任何访问日志, got %q", log.String())
	}
}
