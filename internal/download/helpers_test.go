package download

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
)

// rpcRecorder 记录面板发到 aria2 的每一个 JSON-RPC 请求，供断言"请求体
// 到底长什么样"。用真的 httptest server 注入而不是 mock 掉客户端接口：
// 这一层的价值恰恰在于**它产生的是合法 JSON-RPC 2.0**（含 auth 参数的
// 位置、method 大小写、jsonrpc/id 字段），把 HTTP 换掉就等于什么都没测。
type rpcRecorder struct {
	mu    sync.Mutex
	calls []RPCRequest
	hdr   []http.Header
}

// rpcError 让测试指定"aria2 这次返回 RPC 错误"。
type rpcError struct {
	code int
	msg  string
}

func (r *rpcRecorder) serve(t *testing.T, resp func(method string, params []any) any) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		body, _ := io.ReadAll(req.Body)
		var rr RPCRequest
		if err := json.Unmarshal(body, &rr); err != nil {
			t.Errorf("请求体不是合法 JSON: %v (%s)", err, body)
			w.WriteHeader(400)
			return
		}
		r.mu.Lock()
		r.calls = append(r.calls, rr)
		r.hdr = append(r.hdr, req.Header.Clone())
		r.mu.Unlock()
		out := map[string]any{"jsonrpc": "2.0", "id": rr.ID}
		if resp == nil {
			out["error"] = map[string]any{"code": -32601, "message": "Method not found"}
		} else {
			switch res := resp(rr.Method, rr.Params).(type) {
			case rpcError:
				out["error"] = map[string]any{"code": res.code, "message": res.msg}
			default:
				out["result"] = res
			}
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(out)
	})
}

func (r *rpcRecorder) last() (RPCRequest, http.Header) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.calls) == 0 {
		return RPCRequest{}, http.Header{}
	}
	return r.calls[len(r.calls)-1], r.hdr[len(r.hdr)-1]
}

func (r *rpcRecorder) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.calls)
}

// newTestClient 建一个连到假 aria2 的 Client。resp 决定"aria2 对每个方法
// 回什么"，nil 表示"这个 method 不存在"（回 JSON-RPC -32601）。
func newTestClient(t *testing.T, resp func(method string, params []any) any, secret string) (*Client, *rpcRecorder) {
	t.Helper()
	rec := &rpcRecorder{}
	srv := httptest.NewServer(rec.serve(t, resp))
	t.Cleanup(srv.Close)
	c, err := NewClient(srv.URL, secret)
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	t.Cleanup(c.Close)
	return c, rec
}
