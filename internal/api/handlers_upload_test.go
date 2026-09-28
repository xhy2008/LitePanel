package api_test

// M6-T2：分块上传的 HTTP 层（设计 8.2 + 17 节）。
//
// 与文件页其他接口一样，这里用**真的** filemgr.Service 而不是替身：上传
// 的契约（"缺哪些块"、完成、冲突）全部落在文件系统上，替身只能复述测试
// 自己的想象。替身只出现在"接口报错怎么映射状态码"那一条里。
//
// 设计 17 节只列了 POST /api/fs/upload（带 X-Upload-Id / X-Chunk-Index）,
// 但一个分块请求带的是二进制 body，装不下"传到哪个目录、文件叫什么、
// 总共多大、同名怎么办"这四件必须在第一块之前就定下来的事。所以多一个
// POST /api/fs/upload/begin。不是发明需求，是那份清单少写了一步。

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"litepanel/internal/api"
	"litepanel/internal/filemgr"
)

// ---------- 本文件助手 ----------

type uploadHarness struct {
	t    *testing.T
	h    *harness
	dir  string // 目标目录（真实、已解析链接）
	root string // 暂存根
}

func setUpUpload(t *testing.T) *uploadHarness {
	t.Helper()
	base := t.TempDir()
	real, err := filepath.EvalSymlinks(base)
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(real, "target")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(real, ".lp-upload")
	uh := &uploadHarness{t: t, dir: dir, root: root}
	uh.h = newHarnessWith(t, func(d *api.AuthDeps) {
		d.Files = filemgr.NewService(filemgr.Options{
			ProcDir: "/proc", UploadRoot: root,
			// 小限值：让"超上限"这条路径能在毫秒级测到，不必真造 1GB。
			MaxChunkBytes: 1024, MaxUploadBytes: 4096,
			UploadTTL: time.Hour,
		})
	})
	return uh
}

// raw 发一个带自定义头的请求（分块上传的参数全在头里，没有 JSON 体）。
func (u *uploadHarness) raw(method, path string, headers map[string]string, body io.Reader) *http.Response {
	u.t.Helper()
	req := httptest.NewRequest(method, path, body)
	req.AddCookie(&http.Cookie{Name: cookieName, Value: u.h.token})
	req.Header.Set("X-Requested-With", csrf)
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	u.h.handler.ServeHTTP(w, req)
	return w.Result()
}

func (u *uploadHarness) begin(body string) *http.Response {
	u.t.Helper()
	return u.raw(http.MethodPost, "/api/fs/upload/begin", map[string]string{}, strings.NewReader(body))
}

func (u *uploadHarness) chunk(id string, index any, body string) *http.Response {
	u.t.Helper()
	return u.chunkReader(id, index, strings.NewReader(body))
}

func (u *uploadHarness) chunkReader(id string, index any, body io.Reader) *http.Response {
	u.t.Helper()
	return u.raw(http.MethodPost, "/api/fs/upload", map[string]string{
		"X-Upload-Id": id, "X-Chunk-Index": fmt.Sprint(index),
	}, body)
}

func (u *uploadHarness) status(id string) *http.Response {
	u.t.Helper()
	return u.raw(http.MethodGet, "/api/fs/upload/"+id+"/status", nil, nil)
}

func (u *uploadHarness) abort(id string) *http.Response {
	u.t.Helper()
	return u.raw(http.MethodDelete, "/api/fs/upload/"+id, nil, nil)
}

// beginJSON 是 POST /api/fs/upload/begin 的请求体（字段名即 JSON 契约）。
func beginJSON(id, dir, name string, size, chunk int64, conflict string) string {
	b, _ := json.Marshal(map[string]any{
		"id": id, "dir": dir, "name": name,
		"size": size, "chunk_size": chunk, "conflict": conflict,
	})
	return string(b)
}

func uploadContent(n int) string {
	var b strings.Builder
	for i := 0; i < n; i++ {
		b.WriteByte(byte('a' + i%26))
	}
	return b.String()
}

func uploadChunkOf(full string, i, size int) string {
	s := i * size
	if s >= len(full) {
		return ""
	}
	e := s + size
	if e > len(full) {
		e = len(full)
	}
	return full[s:e]
}

func uploadChunks(full string, size int) int {
	n := len(full) / size
	if len(full)%size != 0 {
		n++
	}
	return n
}

// readState 解出 UploadState。
func readState(t *testing.T, res *http.Response) filemgr.UploadState {
	t.Helper()
	var st filemgr.UploadState
	decodeBody(t, res, &st)
	return st
}

// errFields 一次读全错误体的三个字段。
//
// 有了它就不再"先 assertErrorCode 再 decodeBody"：响应体是一次性流，
// 第二次读得到的是 EOF，而报错会说"响应体不是 JSON"，把一个断言写错
// 伪装成服务端坏了 —— 我在这个文件里就是这么被骗过一次，才加的它。
func errFields(t *testing.T, res *http.Response) (code, message, detail string) {
	t.Helper()
	var b struct {
		Code    string `json:"code"`
		Message string `json:"message"`
		Detail  string `json:"detail"`
	}
	decodeBody(t, res, &b)
	return b.Code, b.Message, b.Detail
}

// readBody 把响应体当文本读掉，只给失败消息用。
func readBody(t *testing.T, res *http.Response) string {
	t.Helper()
	defer res.Body.Close()
	b, _ := io.ReadAll(res.Body)
	return string(b)
}

// namesIn 列目录名。目录不存在时回 nil：很多断言问的是“不该有任何
// 东西”，而不存在正是其中一种。
func namesIn(dir string) []string {
	es, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	out := make([]string, 0, len(es))
	for _, e := range es {
		out = append(out, e.Name())
	}
	return out
}

// ---------- 建立会话 ----------

// 建立会话后按序传块，文件要原样落在目标目录里。
//
// 端到端（HTTP 进、磁盘出）而不是只测 handler 调没调 domain：上传最容易
// 出错的地方正是"参数在头/体之间搬错"，只有看最终字节才知道整条链是通的。
func TestUploadBeginThenChunks(t *testing.T) {
	u := setUpUpload(t)
	full := uploadContent(100)
	resp := u.begin(beginJSON("u1", u.dir, "报告 2026.bin", int64(len(full)), 30, ""))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("begin 应 200, got %d %s", resp.StatusCode, readBody(t, resp))
	}
	st := readState(t, resp)
	if st.ID != "u1" || len(st.Missing) != 4 || st.Done {
		t.Fatalf("begin 响应不对: %+v", st)
	}
	for i := 0; i < uploadChunks(full, 30); i++ {
		r := u.chunk("u1", i, uploadChunkOf(full, i, 30))
		if r.StatusCode != http.StatusOK {
			t.Fatalf("块 %d 应 200, got %d %s", i, r.StatusCode, readBody(t, r))
		}
		st = readState(t, r)
	}
	if !st.Done {
		t.Fatalf("最后一块应报告完成: %+v", st)
	}
	want := filepath.Join(u.dir, "报告 2026.bin")
	if st.Path != want {
		t.Errorf("Path = %q, 期望 %q", st.Path, want)
	}
	b, err := os.ReadFile(want)
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != full {
		t.Errorf("字节不一致: got %d 字节 want %d", len(b), len(full))
	}
	// 用户目录里不许留面板的临时文件
	es, _ := os.ReadDir(u.dir)
	if len(es) != 1 || es[0].Name() != "报告 2026.bin" {
		t.Errorf("目标目录应只剩最终文件, got %v", namesIn(u.dir))
	}
}

// 完成后 status 回完成态，让任何一端（另一个标签页、刷新后的页面）都能
// 问出"那个上传怎么样了"。
func TestUploadStatusAfterDone(t *testing.T) {
	u := setUpUpload(t)
	full := uploadContent(50)
	u.begin(beginJSON("u2", u.dir, "a.bin", int64(len(full)), 25, ""))
	for i := 0; i < 2; i++ {
		u.chunk("u2", i, uploadChunkOf(full, i, 25)).Body.Close()
	}
	res := u.status("u2")
	if res.StatusCode != http.StatusOK {
		t.Fatalf("status 应 200, got %d", res.StatusCode)
	}
	st := readState(t, res)
	if !st.Done || st.Path == "" || st.Received != int64(len(full)) {
		t.Errorf("完成态不对: %+v", st)
	}
}

// 中途断开后重新 begin 同一个 id，回的是**已收到的进度**，不是清零。
//
// 这条是"断点续传"的全部价值所在：手机切后台、WiFi 抖一下就要重传
// 整个文件的话，功能等于没有。
func TestUploadResumeAcrossRestart(t *testing.T) {
	u := setUpUpload(t)
	full := uploadContent(100)
	u.begin(beginJSON("u3", u.dir, "a.bin", int64(len(full)), 30, "")).Body.Close()
	u.chunk("u3", 0, uploadChunkOf(full, 0, 30)).Body.Close()
	u.chunk("u3", 2, uploadChunkOf(full, 2, 30)).Body.Close()

	// 换一个新的 Service 实例，模拟面板重启：没有任何进程内状态传过去，
	// 只有磁盘上的暂存目录。
	restarted := filemgr.NewService(filemgr.Options{
		UploadRoot: u.root, MaxChunkBytes: 1024, MaxUploadBytes: 4096,
	})
	st, err := restarted.UploadStatus(context.Background(), "u3")
	if err != nil {
		t.Fatal(err)
	}
	if st.Received != 60 {
		t.Errorf("重启后应记得已收 60 字节, got %d", st.Received)
	}
	// HTTP 层同样能问到（这条走的是路由，不是 domain）
	res := u.status("u3")
	if res.StatusCode != http.StatusOK {
		t.Fatalf("status 应 200, got %d", res.StatusCode)
	}
	if got := readState(t, res); got.Received != 60 {
		t.Errorf("HTTP status 应记得进度, got %d", got.Received)
	}
}

// 未知 id 的 status 必须 404，不能回一个"0 字节、全缺"的空状态。
//
// 后者看起来像"还没开始传"，前端会照原方案继续传，最后得到一个谁的
// 会话都没建立的残缺文件。
func TestUploadStatusUnknownID(t *testing.T) {
	u := setUpUpload(t)
	res := u.status("never-seen")
	if res.StatusCode != http.StatusNotFound {
		t.Fatalf("未知 id 应 404, got %d %s", res.StatusCode, readBody(t, res))
	}
	assertErrorCode(t, res, "not_found")
}

// ---------- 参数校验 ----------

// 缺 X-Chunk-Index / 非数字，都算"这个请求本身不合法"，400。
//
// 关键是**不能**默默当成第 0 块：那会让一次"前端漏发头"的 bug 变成
// 一个内容错的文件，而且它还报告成功。
func TestUploadChunkIndexRequired(t *testing.T) {
	u := setUpUpload(t)
	u.begin(beginJSON("u4", u.dir, "a.bin", 100, 30, "")).Body.Close()
	cases := map[string]map[string]string{
		"缺 index": {"X-Upload-Id": "u4"},
		"非数字":     {"X-Upload-Id": "u4", "X-Chunk-Index": "abc"},
		"负数":      {"X-Upload-Id": "u4", "X-Chunk-Index": "-1"},
		"越界":      {"X-Upload-Id": "u4", "X-Chunk-Index": "99"},
		"缺 id":    {"X-Chunk-Index": "0"},
	}
	for name, hdr := range cases {
		res := u.raw(http.MethodPost, "/api/fs/upload", hdr, strings.NewReader("xxxx"))
		if res.StatusCode != http.StatusBadRequest {
			t.Errorf("%s 应 400, got %d %s", name, res.StatusCode, readBody(t, res))
			continue
		}
		res.Body.Close()
	}
	// 一次都没被接受，所以不该有任何块落盘、更不该有最终文件
	if names := namesIn(u.dir); len(names) != 0 {
		t.Errorf("非法请求不该产出任何文件: %v", names)
	}
}

// id 里的路径分隔符必须在**进 domain 之前**就被挡掉。
//
// domain 层也挡（cleanUploadID），但那是第二道：HTTP 层放行一个带 ../
// 的 id，得到的是 domain 的 ErrBadPath → 400，看起来也对，区别只在于
// "暂存目录已经被人造出来了没有"。这里断言的是暂存根下什么都没多出。
func TestUploadIDCharsetEnforced(t *testing.T) {
	u := setUpUpload(t)
	for _, id := range []string{"../../etc", "a/b", "..", "."} {
		res := u.begin(beginJSON(id, u.dir, "x.bin", 10, 5, ""))
		if res.StatusCode != http.StatusBadRequest {
			t.Errorf("id %q 应 400, got %d %s", id, res.StatusCode, readBody(t, res))
			continue
		}
		res.Body.Close()
	}
	es, err := os.ReadDir(u.root)
	if err != nil {
		// 会话目录一个都没建 → 暂存根甚至还没被创建，这更好
		return
	}
	if len(es) != 0 {
		t.Errorf("非法 id 不该在暂存根下留下任何东西: %v", namesIn(u.root))
	}
}

// 同名冲突策略里"询问"= 目标已存在时立刻 409，一个字节都不用传。
//
// 为什么要在 begin 就问：选"询问"却等到传完 1GB 才报冲突，用户的 1GB
// 白传了 —— 而这本来可以在第一块之前 0 成本地告诉他。
func TestUploadBeginConflictAsk(t *testing.T) {
	u := setUpUpload(t)
	writeFile(t, filepath.Join(u.dir, "a.bin"), "已有内容")
	res := u.begin(beginJSON("u5", u.dir, "a.bin", 100, 30, ""))
	if res.StatusCode != http.StatusConflict {
		t.Fatalf("冲突应 409, got %d %s", res.StatusCode, readBody(t, res))
	}
	assertErrorCode(t, res, "exists")
	if b, _ := os.ReadFile(filepath.Join(u.dir, "a.bin")); string(b) != "已有内容" {
		t.Error("409 不该动原文件")
	}
}

// 超过单次上传上限回 413，而且文案里要写明走 SFTP。
//
// 只回"太大了"是不够的：用户不知道多大算大、也不知道该怎么办，
// 只会反复点同一个按钮。上限本身是产品决定（网页版是辅助功能）。
func TestUploadTooLarge(t *testing.T) {
	u := setUpUpload(t)
	res := u.begin(beginJSON("u6", u.dir, "big.bin", 4097, 1024, ""))
	if res.StatusCode != http.StatusRequestEntityTooLarge {
		t.Fatalf("超上限应 413, got %d %s", res.StatusCode, readBody(t, res))
	}
	code, message, _ := errFields(t, res)
	if code != "too_large" {
		t.Errorf("code 应 too_large, got %q", code)
	}
	if !strings.Contains(message, "SFTP") && !strings.Contains(message, "sftp") {
		t.Errorf("413 的文案要给出路, got %q", message)
	}
}

// 单块超过服务端上限回 413。
//
// 与上一条不同点在于它**绕过**了 begin 的自查：begin 只校验 size 与
// chunk_size 的声明值，而客户端可以在 chunk 请求里塞一个更长的 body。
func TestUploadChunkTooLarge(t *testing.T) {
	u := setUpUpload(t)
	// size 必须**不超**整次上限，否则 begin 就先 413 了，chunk 端拿到
	// 的是"会话不存在"的 404 —— 那测的完全不是单块上限。
	resp := u.begin(beginJSON("u7", u.dir, "a.bin", 4096, 1024, ""))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("begin 应 200, got %d %s", resp.StatusCode, readBody(t, resp))
	}
	res := u.chunk("u7", 0, strings.Repeat("x", 2048))
	if res.StatusCode != http.StatusRequestEntityTooLarge {
		t.Fatalf("超大块应 413, got %d %s", res.StatusCode, readBody(t, res))
	}
	assertErrorCode(t, res, "chunk_too_large")
}

// 块长度与序号不符 → 400。
func TestUploadChunkWrongLength(t *testing.T) {
	u := setUpUpload(t)
	u.begin(beginJSON("u8", u.dir, "a.bin", 100, 30, "")).Body.Close()
	res := u.chunk("u8", 0, "太短了")
	if res.StatusCode != http.StatusBadRequest {
		t.Fatalf("块长不符应 400, got %d %s", res.StatusCode, readBody(t, res))
	}
	assertErrorCode(t, res, "chunk_length")
}

// 同一块重传不同内容 → 409，而不是静默采用后到的。
//
// 这条能测到，说明"客户端算错块"与"网络传坏"会被当作事故上报，
// 而不是产出一个内容鬼掉的文件。
func TestUploadChunkMismatch(t *testing.T) {
	u := setUpUpload(t)
	full := uploadContent(100)
	u.begin(beginJSON("u9", u.dir, "a.bin", int64(len(full)), 30, "")).Body.Close()
	u.chunk("u9", 0, uploadChunkOf(full, 0, 30)).Body.Close()
	bad := []byte(uploadChunkOf(full, 0, 30))
	bad[0] ^= 0xff
	res := u.chunk("u9", 0, string(bad))
	if res.StatusCode != http.StatusConflict {
		t.Fatalf("同块不同内容应 409, got %d %s", res.StatusCode, readBody(t, res))
	}
	assertErrorCode(t, res, "chunk_mismatch")
}

// ---------- 冲突策略 ----------

// skip 策略：begin 直接回"跳过"，一个字节都不传。
func TestUploadSkip(t *testing.T) {
	u := setUpUpload(t)
	writeFile(t, filepath.Join(u.dir, "a.bin"), "原有内容")
	res := u.begin(beginJSON("sk", u.dir, "a.bin", 100, 30, "skip"))
	if res.StatusCode != http.StatusOK {
		t.Fatalf("skip 应 200, got %d %s", res.StatusCode, readBody(t, res))
	}
	st := readState(t, res)
	if !st.Skipped || !st.Done {
		t.Errorf("应回 Skipped+Done: %+v", st)
	}
	if names := namesIn(u.dir); len(names) != 1 || names[0] != "a.bin" {
		t.Errorf("skip 不该产出任何东西: %v", names)
	}
}

// rename 策略：无冲突时用原名，有冲突时才让位。
//
// "让位"策略最常见的写坏方式是**一律**加后缀：上传一个全新文件也得
// 到 a (1).bin，用户第二天看到的是满屏自己没起过的名字。
func TestUploadRenamePolicy(t *testing.T) {
	u := setUpUpload(t)
	full := uploadContent(40)
	// 全新文件：不该有后缀
	r := u.begin(beginJSON("rn1", u.dir, "a.bin", int64(len(full)), 20, "rename"))
	st := readState(t, r)
	if st.Name != "a.bin" {
		t.Fatalf("无冲突时该用原名, got %q", st.Name)
	}
	r.Body.Close()
	for i := 0; i < 2; i++ {
		u.chunk("rn1", i, uploadChunkOf(full, i, 20)).Body.Close()
	}
	// 同名出现了：让位。
	//
	// 断言点在**完成之后**，不是 begin：最终名要到收尾时才定，因为
	// "同名"可能在上传中途才出现（另一个标签页、用户自己在终端里
	// cp）。begin 时预告一个名字，中途被抢占后就变成一句谎话 —— 而
	// 完成响应里的 Path 才是"它到底落在哪"的唯一权威答案。
	r2 := u.begin(beginJSON("rn2", u.dir, "a.bin", int64(len(full)), 20, "rename"))
	r2.Body.Close()
	var last filemgr.UploadState
	for i := 0; i < 2; i++ {
		last = readState(t, u.chunk("rn2", i, uploadChunkOf(full, i, 20)))
	}
	if !last.Done {
		t.Fatalf("应已完成: %+v", last)
	}
	if filepath.Base(last.Path) != "a (1).bin" {
		t.Errorf("有冲突时该让位, got %q", last.Path)
	}
	if names := namesIn(u.dir); len(names) != 2 {
		t.Errorf("应有两个文件: %v", names)
	}
	// 让位后的名字必须真的能查到（不是只存在于响应里）
	if _, err := os.Stat(filepath.Join(u.dir, "a (1).bin")); err != nil {
		t.Errorf("让位后的文件不在盘上: %v", err)
	}
}

// ---------- 取消 ----------

// DELETE 之后再 status 必须 404。
//
// 只测 204 是不够的：取消的真意是"这些东西别再留在盘上"，而"取消了
// 但状态还在"是最容易被前端读成"还在传"的状态。
func TestUploadAbort(t *testing.T) {
	u := setUpUpload(t)
	u.begin(beginJSON("ab", u.dir, "a.bin", 100, 30, "")).Body.Close()
	u.chunk("ab", 0, uploadChunkOf(uploadContent(100), 0, 30)).Body.Close()
	res := u.abort("ab")
	if res.StatusCode != http.StatusNoContent {
		t.Fatalf("取消应 204, got %d %s", res.StatusCode, readBody(t, res))
	}
	if got := u.status("ab"); got.StatusCode != http.StatusNotFound {
		t.Errorf("取消后 status 应 404, got %d", got.StatusCode)
		got.Body.Close()
	}
	if names := namesIn(u.root); len(names) != 0 {
		t.Errorf("取消后暂存该清空: %v", names)
	}
}

// 取消一个**已完成**的上传必须被拒绝，而且要说清用什么替代。
//
// 文件已经落在用户目录里了。这里若回 204，界面显示"取消成功"，而磁盘
// 上多一个文件 —— 那是比报错更糟的谎。
func TestUploadAbortAfterDone(t *testing.T) {
	u := setUpUpload(t)
	full := uploadContent(40)
	u.begin(beginJSON("dn", u.dir, "a.bin", int64(len(full)), 20, "")).Body.Close()
	for i := 0; i < 2; i++ {
		u.chunk("dn", i, uploadChunkOf(full, i, 20)).Body.Close()
	}
	res := u.abort("dn")
	if res.StatusCode != http.StatusConflict {
		t.Fatalf("已完成的上传取消不了，应 409, got %d %s", res.StatusCode, readBody(t, res))
	}
	// code 必须是 upload_done 而不是 exists：前端按 code 决定动作，
	// 而这里正确的动作是"去删除"，不是"换个名字重试"。
	assertErrorCode(t, res, "upload_done")
	if _, err := os.Stat(filepath.Join(u.dir, "a.bin")); err != nil {
		t.Errorf("拒绝取消时不该动文件: %v", err)
	}
}

// ---------- 鉴权与 CSRF ----------

// 上传端点一律要登录 + CSRF 头。
//
// 值得单独钉：分块接口用**头**传参，很多人会以为"没有 JSON 体所以没有
// CSRF 风险"，于是漏掉这层。事实相反 —— 一个跨站表单发不出自定义头，
// 正是靠这个头挡住的；漏掉它，任何网页都能往服务器写文件。
func TestUploadRequiresAuthAndCSRF(t *testing.T) {
	u := setUpUpload(t)
	// 未登录（GET 路径：没 CSRF 中间件拦，能走到鉴权）
	req := httptest.NewRequest(http.MethodGet, "/api/fs/upload/x/status", nil)
	w := httptest.NewRecorder()
	u.h.handler.ServeHTTP(w, req)
	if w.Result().StatusCode != http.StatusUnauthorized {
		t.Errorf("未登录应 401, got %d", w.Result().StatusCode)
	}
	// 已登录但没 CSRF 头
	noCSRF := httptest.NewRequest(http.MethodPost, "/api/fs/upload", strings.NewReader("x"))
	noCSRF.AddCookie(&http.Cookie{Name: cookieName, Value: u.h.token})
	w2 := httptest.NewRecorder()
	u.h.handler.ServeHTTP(w2, noCSRF)
	if w2.Result().StatusCode != http.StatusForbidden {
		t.Errorf("缺 CSRF 头应 403, got %d", w2.Result().StatusCode)
	}
	// 上面的 401 只适用于 GET：全局 CSRF 中间件在鉴权**之前**，所以裸
	// POST/DELETE 一律先拿 403。这不是把“未登录”说成了“CSRF 错”：
	// 两个响应都不含任何业务信息，而“先验证请求是不是同源”本来就该排在
	// “验证你是谁”前面 —— 一个连同源都不是的请求，不值得先去查 session。
	for _, m := range []struct{ method, path string }{
		{http.MethodPost, "/api/fs/upload/begin"},
		{http.MethodGet, "/api/fs/upload/x/status"},
		{http.MethodDelete, "/api/fs/upload/x"},
	} {
		r := httptest.NewRequest(m.method, m.path, nil)
		ww := httptest.NewRecorder()
		u.h.handler.ServeHTTP(ww, r)
		if m.method == http.MethodGet {
			if ww.Result().StatusCode != http.StatusUnauthorized {
				t.Errorf("%s %s 未登录应 401, got %d", m.method, m.path, ww.Result().StatusCode)
			}
			continue
		}
		if ww.Result().StatusCode != http.StatusForbidden {
			t.Errorf("%s %s 未登录且无 CSRF 头应先 403, got %d", m.method, m.path, ww.Result().StatusCode)
		}
	}
}

// ---------- 错误映射 ----------

// domain 的未知错误回 500 并带 detail，不能伪装成客户端错误。
//
// 本文件唯一用替身处：真实文件系统造不出一个"不属于任何已知哨兵"的错误，
// 而"未知归 500"是排查远程故障时唯一能看到原始文本的地方。
func TestUploadUnknownErrorMapsTo500(t *testing.T) {
	boom := errors.New("某种没见过的内核错误")
	h := newHarnessWith(t, func(d *api.AuthDeps) { d.Files = stubUpload{boom} })
	res := h.do(http.MethodGet, "/api/fs/upload/x/status", true, "", "", "")
	if res.StatusCode != http.StatusInternalServerError {
		t.Fatalf("未知错误应 500, got %d %s", res.StatusCode, readBody(t, res))
	}
	_, _, detail := errFields(t, res)
	if !strings.Contains(detail, boom.Error()) {
		t.Errorf("detail 要带上原始错误, got %q", detail)
	}
}

// stubUpload 只把每个方法变成一个没见过的错误。
type stubUpload struct{ err error }

func (s stubUpload) List(context.Context, string, filemgr.ListOptions) (filemgr.ListPage, error) {
	return filemgr.ListPage{}, s.err
}
func (s stubUpload) Stat(context.Context, string) (filemgr.Entry, error) {
	return filemgr.Entry{}, s.err
}
func (s stubUpload) Mkdir(context.Context, string) error { return s.err }
func (s stubUpload) Rename(context.Context, string, string) error {
	return s.err
}
func (s stubUpload) Roots(context.Context) ([]filemgr.Root, error) { return nil, s.err }
func (s stubUpload) Open(context.Context, string) (filemgr.Opened, error) {
	return filemgr.Opened{}, s.err
}
func (s stubUpload) Zip(context.Context, []string, io.Writer) error { return s.err }
func (s stubUpload) BeginUpload(context.Context, filemgr.UploadInit) (filemgr.UploadState, error) {
	return filemgr.UploadState{}, s.err
}
func (s stubUpload) PutChunk(context.Context, filemgr.UploadChunk) (filemgr.UploadState, error) {
	return filemgr.UploadState{}, s.err
}
func (s stubUpload) UploadStatus(context.Context, string) (filemgr.UploadState, error) {
	return filemgr.UploadState{}, s.err
}
func (s stubUpload) AbortUpload(context.Context, string) error { return s.err }

// ---------- 装配兜底 ----------

// Files 没接时上传端点必须 501，不能 200 + 空。
//
// 与文件页其他接口同一条理由：空状态会被前端渲染成"没有正在进行的上传"，
// 把"面板没接模块"说成"你什么都没传"。
func TestUploadReturns501WithoutFiles(t *testing.T) {
	h := newHarness(t)
	for _, r := range []struct{ method, path string }{
		{http.MethodPost, "/api/fs/upload/begin"},
		{http.MethodGet, "/api/fs/upload/x/status"},
		{http.MethodDelete, "/api/fs/upload/x"},
	} {
		req := httptest.NewRequest(r.method, r.path, nil)
		req.AddCookie(&http.Cookie{Name: cookieName, Value: h.token})
		req.Header.Set("X-Requested-With", csrf)
		w := httptest.NewRecorder()
		h.handler.ServeHTTP(w, req)
		if w.Result().StatusCode != http.StatusNotImplemented {
			t.Errorf("%s %s 未装配时应 501, got %d", r.method, r.path, w.Result().StatusCode)
		}
	}
}

// 无限流的请求体必须**很快**被挡下，而不是把 worker 挂住。
//
// 上限检查写成"先收完再比长度"的话，一个恶意/抽风的客户端可以让面板
// 一直读下去：磁盘写满、worker 永不归还，而且它一个字节都不用多发明
// —— 只要不关闭连接。这条测试用"永远读不到 EOF 的 body"来区分两种
// 实现：按上限停的立刻回 413；读完再比的会把夹具那 8MB 全写完，
// 最后回一个别的状态码。
func TestUploadChunkBodyStreamCapped(t *testing.T) {
	u := setUpUpload(t)
	resp := u.begin(beginJSON("inf", u.dir, "a.bin", 4096, 1024, ""))
	resp.Body.Close()
	done := make(chan *http.Response, 1)
	go func() { done <- u.chunkReader("inf", 0, endlessBody{}) }()
	var res *http.Response
	select {
	case res = <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("无限流请求体没有被挡下：上限检查发生在了读取之后")
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusRequestEntityTooLarge {
		t.Errorf("应 413, got %d", res.StatusCode)
	}
}

// endlessBody 永远读不到 EOF，但超过约 8MB 就报错 —— 万一实现是
// "读完了才比长度"，测试仍能收尾（并以错误的状态码失败），而不是
// 挂到 go test 的全局超时。
type endlessBody struct{ n int }

func (e endlessBody) Read(p []byte) (int, error) {
	if e.n > 8<<20 {
		return 0, io.ErrUnexpectedEOF
	}
	for i := range p {
		p[i] = 'x'
	}
	e.n += len(p)
	return len(p), nil
}

// 客户端中途断开不是服务端的错，回 499。
//
// 关页面、切后台、超时都会走到这里，频率不低。如果它被归成 500，
// 运维看日志时满屏都是"上传失败"，而真正坏掉的那一两条被淹没了 ——
// 499 的价值全在于让"用户不传了"与"面板坏了"在日志里分得开。
// 会话本身要留着：用户回来还能续传。
func TestUploadChunkCanceledClient(t *testing.T) {
	u := setUpUpload(t)
	u.begin(beginJSON("cx", u.dir, "a.bin", 4096, 1024, "")).Body.Close()
	res := u.chunkReader("cx", 0, canceledBody{})
	defer res.Body.Close()
	if res.StatusCode != 499 {
		t.Fatalf("客户端断开应 499, got %d %s", res.StatusCode, readBody(t, res))
	}
	assertErrorCode(t, res, "canceled")
	// 会话还在（下一次能续传）
	st := u.status("cx")
	if st.StatusCode != http.StatusOK {
		t.Errorf("取消一次不该删掉会话, status got %d", st.StatusCode)
	} else {
		st.Body.Close()
	}
}

type canceledBody struct{}

func (canceledBody) Read([]byte) (int, error) { return 0, context.Canceled }
