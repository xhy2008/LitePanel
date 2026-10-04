package download

import (
	"context"
	"errors"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// fakeProber 是可控的 aria2 探活对象。
type fakeProber struct {
	ver string
	err error
	n   atomic.Int64
}

func (f *fakeProber) GetVersion(context.Context) (string, error) {
	f.n.Add(1)
	return f.ver, f.err
}

func TestHealthReportsVersionWhenReachable(t *testing.T) {
	p := &fakeProber{ver: "1.37.0"}
	h := NewHealthChecker(p, time.Minute)
	got := h.Check(context.Background())
	if !got.OK || got.Version != "1.37.0" {
		t.Fatalf("可达时应报版本号, got %+v", got)
	}
	if got.Message != "" {
		t.Errorf("健康时不该带 message, got %q", got.Message)
	}
}

// 不可达时给的是**下一步动作**，不是一个错误码。aria2 是可选依赖，"没装"
// 是绝大多数用户的实际处境 —— 只说 "connection refused" 等于让用户自己去
// 猜该装什么包。
func TestHealthUnavailableGivesActionableMessage(t *testing.T) {
	p := &fakeProber{err: &unavailableError{err: errors.New("connection refused")}}
	h := NewHealthChecker(p, time.Minute)
	got := h.Check(context.Background())
	if got.OK {
		t.Fatal("连不上不能算健康")
	}
	msg := got.Message
	if msg == "" {
		t.Fatal("不可达必须带一句给用户的话（前端直接显示）")
	}
	if !strings.Contains(msg, "aria2") {
		t.Errorf("要说清是 aria2 不在了, got %q", msg)
	}
	// 必须落到"给下一步动作"的分支，而不是把原始错误直接抄出来。
	// 只断言"含 aria2"是不够的：默认分支的 err.Error() 恰好也以 "aria2 不可达"
	// 开头，两种写法都能过 —— 于是分支被删掉时测试仍是绿的。
	if !strings.Contains(msg, "未安装") && !strings.Contains(msg, "未启动") {
		t.Errorf("不可达要给出可操作的猜测（装什么/起什么）, got %q", msg)
	}
	if strings.Contains(msg, "connection refused") {
		t.Errorf("不该把 Go 的原始错误直接甩给用户: %q", msg)
	}
}

// 连上了但 RPC 报错（密钥错、端口上跑的别的 JSON-RPC 服务）必须与"没连上"
// 区分：两者的修法完全不同，合并成一句话就等于让一半用户白折腾。
func TestHealthDistinguishesRPCFailureFromUnreachable(t *testing.T) {
	p := &fakeProber{err: &Error{Code: 1, Message: "Unauthorized", Method: "aria2.getVersion"}}
	h := NewHealthChecker(p, time.Minute)
	got := h.Check(context.Background())
	if got.OK {
		t.Fatal("RPC 报错不能算健康")
	}
	if !strings.Contains(got.Message, "Unauthorized") {
		t.Errorf("要留着 aria2 原文供人判断是密钥问题, got %q", got.Message)
	}
	// 反方向也要钉：这类故障**不是**"没装"，如果也提示"可能未安装"，用户会
	// 去 apt install 一遍然后回来发现还是不行。两个分支的文案必须互斥。
	if strings.Contains(got.Message, "未安装") || strings.Contains(got.Message, "未启动") {
		t.Errorf("RPC 有响应时不该提示装/起服务: %q", got.Message)
	}
}

// 探活结果必须缓存。下载页每 2 秒刷新一次；没有缓存时，aria2 没起的那段
// 时间里面板会持续对死端口发起 TCP 连接，而每次都要等到超时才返回 ——
// 面板自己变成故障放大器，还把 HTTP 连接占着。
func TestHealthCachesWithinTTL(t *testing.T) {
	p := &fakeProber{err: &unavailableError{err: errors.New("refused")}}
	h := NewHealthChecker(p, time.Hour)
	for i := 0; i < 5; i++ {
		h.Check(context.Background())
	}
	if n := p.n.Load(); n != 1 {
		t.Errorf("TTL 内应只用缓存（探活 1 次）, got %d 次", n)
	}
}

func TestHealthReprobesAfterTTL(t *testing.T) {
	p := &fakeProber{err: &unavailableError{err: errors.New("refused")}}
	h := NewHealthChecker(p, 10*time.Millisecond)
	h.Check(context.Background())
	time.Sleep(20 * time.Millisecond)
	h.Check(context.Background())
	if n := p.n.Load(); n != 2 {
		t.Errorf("过了 TTL 要重新探（aria2 可能刚被 systemctl start）, got %d 次", n)
	}
}

// Forget 是给"设置页刚改了 RPC 地址"用的：不清缓存的话，用户填对地址之后
// 还要等一个 TTL 才能看到任务列表，而他刚做完的动作看起来毫无效果。
func TestForgetForcesReprobe(t *testing.T) {
	p := &fakeProber{err: &unavailableError{err: errors.New("refused")}}
	h := NewHealthChecker(p, time.Hour)
	h.Check(context.Background())
	h.Forget()
	if got := h.Check(context.Background()); p.n.Load() != 2 {
		t.Errorf("Forget 后必须重探, got %+v (n=%d)", got, p.n.Load())
	}
}

// ctx 取消**不能**进缓存。用户关页面/切标签会取消请求，把"取消"缓存成
// "aria2 坏了"，会让下一个打开下载页的人看到一个假故障，并且要等一整个
// TTL 才自愈。
func TestContextCancelIsNotCachedAsFailure(t *testing.T) {
	p := &fakeProber{err: context.Canceled}
	h := NewHealthChecker(p, time.Hour)
	h.Check(context.Background())
	p.ver, p.err = "1.37.0", nil
	got := h.Check(context.Background())
	if !got.OK {
		t.Errorf("取消不该被当成 aria2 故障缓存下来: %+v", got)
	}
}

// 真实的 Client 走真 HTTP：证明 NewClient + GetVersion + HealthChecker 串起
// 来在"端口没人听"时给出的是 available=false 而不是 panic 或挂住。
func TestHealthOverRealHTTPUnreachable(t *testing.T) {
	srv := httptest.NewServer(nil)
	url := srv.URL
	srv.Close()
	c, err := NewClient(url, "")
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	h := NewHealthChecker(c, time.Minute)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	got := h.Check(ctx)
	if got.OK {
		t.Fatal("端口关着不能算健康")
	}
	if got.Message == "" {
		t.Error("要带一句可操作的话")
	}
}

// ttl<=0 必须有兜底。这个值将来从设置页来，填 0（或配置里漏写）是很常见的
// 手误；没有兜底就退化成"每次刷新都真连一次"，正是上面那条缓存要防的故障
// 放大。兜底成默认值而不是报错：这是配置容错，不是用户可修复的错误。
func TestZeroTTLFallsBackToDefault(t *testing.T) {
	p := &fakeProber{err: &unavailableError{err: errors.New("refused")}}
	h := NewHealthChecker(p, 0)
	h.Check(context.Background())
	h.Check(context.Background())
	if n := p.n.Load(); n != 1 {
		t.Errorf("ttl=0 应回落到默认缓存（只探 1 次）, got %d 次", n)
	}
}
