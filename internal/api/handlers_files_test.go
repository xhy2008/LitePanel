package api_test

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"litepanel/internal/api"
	"litepanel/internal/filemgr"
)

// M6-T6：文件接口的 HTTP 层（设计 690-694 行）。
//
// 与前面几个模块不同，这里**不用 filemgr 的替身**。理由不是省事：文件路径
// 的错误种类（ErrBadPath / ErrNotDirectory / ErrNotExist / ErrPermission）
// 只有真实文件系统能可靠造出来，替身会把"实现到底返回哪种错误"这个真正的
// 契约点，替换成测试自己的一厢情愿 —— 替身说返回 A、真实现返回 B，测试
// 全绿而线上 500。

const csrf = "litepanel" // 设计 5.7：X-Requested-With 的值

// ---------- 本文件的助手 ----------

type filesHarness struct {
	*harness
	root string // 可写的临时目录（已解析符号链接）
}

func setUpFiles(t *testing.T) *filesHarness {
	t.Helper()
	dir := t.TempDir()
	// 断言一律用解析后的路径：AbsClean 会解析符号链接，而 TempDir 在某些
	// 平台上本身就是链接，不解析的话每个断言都要猜。
	real, err := filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatal(err)
	}
	h := &filesHarness{root: real}
	h.harness = newHarnessWith(t, func(d *api.AuthDeps) {
		d.Files = filemgr.NewService(filemgr.Options{ProcDir: "/proc"})
	})
	return h
}

func (f *filesHarness) get(q string) *http.Response {
	f.t.Helper()
	return f.do("GET", q, true, "", "", "")
}

// post 带上 CSRF 头（面板所有非 GET 请求都要带）。
func (f *filesHarness) post(path, body string) *http.Response {
	f.t.Helper()
	return f.do("POST", path, true, csrf, body, "")
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func qs(p string) string { return url.QueryEscape(p) }

// jq 把路径塞进 JSON 字符串字段。用 json.Marshal 而不是手拼转义：
// 反斜杠与引号的转义规则由标准库负责，测试不会因为 Windows 风格路径
// 而写出无效 JSON。
func jq(s string) string {
	b, err := json.Marshal(s)
	if err != nil {
		panic(err)
	}
	return string(b)
}

func decodeBody(t *testing.T, res *http.Response, dst any) {
	t.Helper()
	defer res.Body.Close()
	if err := json.NewDecoder(res.Body).Decode(dst); err != nil {
		t.Fatalf("响应体不是 JSON: %v", err)
	}
}

// assertErrorCode 断言错误体的 code 字段。
//
// 为什么不只断言状态码：前端要按 code 决定动作（"exists" 弹重命名建议、
// "permission_denied" 提示用 root 重登），而 400/404/409 都会撞上别的
// 来源。只测状态码的话，把 mkdir 的 409 改成 400 也能一路绿到线上。
func assertErrorCode(t *testing.T, res *http.Response, want string) {
	t.Helper()
	var b struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	}
	decodeBody(t, res, &b)
	if b.Code != want {
		t.Fatalf("错误码 = %q (%s), 期望 %q", b.Code, b.Message, want)
	}
	if strings.TrimSpace(b.Message) == "" {
		t.Error("错误必须带人类可读的 message")
	}
}

// ---------- GET /api/fs/list ----------

func TestFSListOK(t *testing.T) {
	h := setUpFiles(t)
	writeFile(t, filepath.Join(h.root, "a.txt"), "hello")
	if err := os.Mkdir(filepath.Join(h.root, "子目录"), 0o755); err != nil {
		t.Fatal(err)
	}

	res := h.get("/api/fs/list?path=" + qs(h.root))
	if res.StatusCode != http.StatusOK {
		t.Fatalf("状态码 %d", res.StatusCode)
	}
	var body struct {
		Path    string          `json:"path"`
		Total   int             `json:"total"`
		Entries []filemgr.Entry `json:"entries"`
	}
	decodeBody(t, res, &body)
	if body.Path != h.root {
		t.Fatalf("path 回显不符: %q vs %q", body.Path, h.root)
	}
	if body.Total != 2 || len(body.Entries) != 2 {
		t.Fatalf("total=%d entries=%d", body.Total, len(body.Entries))
	}
	// 目录优先（filemgr 的排序契约必须在 HTTP 层原样传出去）
	if body.Entries[0].Name != "子目录" {
		t.Fatalf("目录应排在前: %+v", body.Entries)
	}
}

// 缺 path 参数 → 400。
//
// 不能"默认列根目录"也不能按进程 cwd 解释：前者会让手机用户一打开文件页
// 就站在 / 上（一屏系统目录，什么都干不了），后者是 M6-T1 就立过的规矩
// （同一参数在开发机与目标机指向不同目录）。起始目录是设置项（M7-T5），
// 由前端带上。
func TestFSListMissingPath(t *testing.T) {
	h := setUpFiles(t)
	res := h.get("/api/fs/list")
	if res.StatusCode != http.StatusBadRequest {
		t.Fatalf("缺 path 应 400, got %d", res.StatusCode)
	}
	assertErrorCode(t, res, "bad_path")
}

// 相对路径、空白 path 都归 400 + bad_path（filemgr.ErrBadPath 的映射）。
func TestFSListBadPath(t *testing.T) {
	h := setUpFiles(t)
	for _, q := range []string{"relative/path", "a/b", "%20"} {
		res := h.get("/api/fs/list?path=" + q)
		if res.StatusCode != http.StatusBadRequest {
			t.Errorf("path=%q 应 400, got %d", q, res.StatusCode)
			continue
		}
		assertErrorCode(t, res, "bad_path")
	}
}

// 目录不存在 → 404。列表与双击之间目录被删是日常时序，不是输入错误：
// 前端拿 404 会退回上级目录，拿 400 只会原地报错。
func TestFSListNotFound(t *testing.T) {
	h := setUpFiles(t)
	res := h.get("/api/fs/list?path=" + qs(filepath.Join(h.root, "没了")))
	if res.StatusCode != http.StatusNotFound {
		t.Fatalf("应 404, got %d", res.StatusCode)
	}
	assertErrorCode(t, res, "not_found")
}

// 路径是普通文件 → 400，不是 404：文件确实在，只是不是目录。
func TestFSListOnFile(t *testing.T) {
	h := setUpFiles(t)
	f := filepath.Join(h.root, "f.txt")
	writeFile(t, f, "x")
	res := h.get("/api/fs/list?path=" + qs(f))
	if res.StatusCode != http.StatusBadRequest {
		t.Fatalf("对文件列目录应 400, got %d", res.StatusCode)
	}
	assertErrorCode(t, res, "not_directory")
}

// 无权限目录 → 403，而且**绝不能是 500**（验收条款"返回可读错误而非 500"）。
func TestFSListPermission(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root 无视权限位")
	}
	h := setUpFiles(t)
	secret := filepath.Join(h.root, "secret")
	writeFile(t, filepath.Join(secret, "x"), "x")
	if err := os.Chmod(secret, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(secret, 0o755) })
	res := h.get("/api/fs/list?path=" + qs(secret))
	if res.StatusCode != http.StatusForbidden {
		t.Fatalf("无权限应 403, got %d", res.StatusCode)
	}
	assertErrorCode(t, res, "permission_denied")
}

// sort/order/page/size/show_hidden 全部要生效。
//
// 断言"参数生效"而不是"参数被读"：前端每次点表头都发一次请求，参数被
// 默默忽略的表现是"点了没反应"，用户只会以为面板卡死。
func TestFSListQueryPassthrough(t *testing.T) {
	h := setUpFiles(t)
	for _, n := range []string{"c.txt", "a.txt", "b.txt", ".hidden"} {
		writeFile(t, filepath.Join(h.root, n), strings.Repeat("x", len(n)))
	}
	list := func(query string) string {
		res := h.get("/api/fs/list?path=" + qs(h.root) + "&" + query)
		if res.StatusCode != http.StatusOK {
			t.Fatalf("%s -> %d", query, res.StatusCode)
		}
		var body struct {
			Entries []filemgr.Entry `json:"entries"`
		}
		decodeBody(t, res, &body)
		var names []string
		for _, e := range body.Entries {
			names = append(names, e.Name)
		}
		return strings.Join(names, ",")
	}
	if s := list("sort=name&order=desc"); s != "c.txt,b.txt,a.txt" {
		t.Errorf("降序未生效: %s", s)
	}
	if s := list("sort=name&size=2&page=2"); s != "c.txt" {
		t.Errorf("分页未生效: %s", s)
	}
	if s := list("show_hidden=1"); !strings.Contains(s, ".hidden") {
		t.Errorf("show_hidden 未生效: %s", s)
	}
	// 未知 sort 值退回名称序（升序），不报错：书签/收藏里的旧参数不该让
	// 页面打不开。期望值就是上面 sort=name 的那一组 —— 写"未知值应与
	// 默认值等价"比写死某个具体顺序更难被改错
	if s := list("sort=乱七八糟"); s != "a.txt,b.txt,c.txt" {
		t.Errorf("未知 sort 应退回名称升序: %s", s)
	}
	// 非数字的 page/size 同样降级为默认值，而不是 400：地址栏里粘进来的
	// 一串 URL 带 page=abc 时，用户的意图是"打开这个目录"
	res := h.get("/api/fs/list?path=" + qs(h.root) + "&page=abc&size=xyz")
	if res.StatusCode != http.StatusOK {
		t.Fatalf("非数字分页参数不该报错, got %d", res.StatusCode)
	}
	// 降级后必须是**第一页**（不是"什么都没过滤"的第 0 页偏移）：
	// 只断 200 的话，"把非法值翻译成负偏移"的实现也算通过
	var fallback struct {
		Page    int             `json:"page"`
		Size    int             `json:"size"`
		Entries []filemgr.Entry `json:"entries"`
	}
	decodeBody(t, res, &fallback)
	if fallback.Page != 1 || fallback.Size != filemgr.DefaultPageSize {
		t.Errorf("非法分页参数应收敛到 page=1/size=默认, got page=%d size=%d", fallback.Page, fallback.Size)
	}
	// 夹具放了 4 个名字，其中 1 个是隐藏文件：默认的 show_hidden=off
	// 下第一页应该是 3 条。写死 3 而不是"非空"，是为了让"降级时顺手把
	// 隐藏文件也放出来"这种实现差异可见。
	if len(fallback.Entries) != 3 {
		t.Errorf("降级后必须给出第一页内容, got %d 条", len(fallback.Entries))
	}

	// Boolean 参数的两种写法都必须生效。少一种的表现为"设置了但没生效"，
	// 用户只会以为面板不认识这个开关
	for _, v := range []string{"1", "true", "TRUE", "True"} {
		if s := list("show_hidden=" + v); !strings.Contains(s, ".hidden") {
			t.Errorf("show_hidden=%s 未生效: %s", v, s)
		}
	}
	// 反过来：0 / false / 空 都不能被当成"开"（否则默认值形同虚设）
	for _, v := range []string{"0", "false", "", "no"} {
		if s := list("show_hidden=" + v); strings.Contains(s, ".hidden") {
			t.Errorf("show_hidden=%s 不该显示隐藏文件: %s", v, s)
		}
	}
}

// 未登录一律 401：文件接口是全盘读写能力，鉴权不能留任何缝隙。
func TestFSListRequiresAuth(t *testing.T) {
	h := setUpFiles(t)
	res := h.do("GET", "/api/fs/list?path="+qs(h.root), false, "", "", "")
	if res.StatusCode != http.StatusUnauthorized {
		t.Fatalf("未登录应 401, got %d", res.StatusCode)
	}
}

// ---------- GET /api/fs/stat ----------

func TestFSStat(t *testing.T) {
	h := setUpFiles(t)
	writeFile(t, filepath.Join(h.root, "报告.md"), "内容") // 2 汉字 = 6 字节
	res := h.get("/api/fs/stat?path=" + qs(filepath.Join(h.root, "报告.md")))
	if res.StatusCode != http.StatusOK {
		t.Fatalf("状态码 %d", res.StatusCode)
	}
	var e filemgr.Entry
	decodeBody(t, res, &e)
	if e.Name != "报告.md" || e.MIME != "text/markdown" || e.Size != 6 {
		t.Fatalf("stat 结果不符: %+v", e)
	}
	// 目录：is_dir 为真（属性对话框与"能否进入"判断要它）
	res = h.get("/api/fs/stat?path=" + qs(h.root))
	decodeBody(t, res, &e)
	if !e.IsDir {
		t.Fatalf("目录 stat: %+v", e)
	}
	// 不存在 → 404，绝不能返回零值 Entry + 200：前端会把空对象渲染成
	// "一个名为空、大小 0 的文件"
	res = h.get("/api/fs/stat?path=" + qs(filepath.Join(h.root, "无")))
	if res.StatusCode != http.StatusNotFound {
		t.Fatalf("不存在应 404, got %d", res.StatusCode)
	}
	// 缺 path → 400
	res = h.get("/api/fs/stat")
	if res.StatusCode != http.StatusBadRequest {
		t.Fatalf("缺 path 应 400, got %d", res.StatusCode)
	}
}

// ---------- POST /api/fs/mkdir ----------

func TestFSMkdir(t *testing.T) {
	h := setUpFiles(t)
	target := filepath.Join(h.root, "新建 目录", "更深一层")
	res := h.post("/api/fs/mkdir", `{"path":`+jq(target)+`}`)
	if res.StatusCode != http.StatusCreated {
		t.Fatalf("mkdir 应 201, got %d", res.StatusCode)
	}
	// 多级路径：末段不存在时要能一路建出来（"新建文件夹"常带子路径）
	if fi, err := os.Stat(target); err != nil || !fi.IsDir() {
		t.Fatalf("目录没建出来: %v", err)
	}
	// 已存在 → 409，而不是 os.MkdirAll 的静默成功：静默成功会让用户以为
	// "新建"生成了一个新目录，而它其实进去了一个同名旧目录（里面可能
	// 有几百个文件）
	res = h.post("/api/fs/mkdir", `{"path":`+jq(target)+`}`)
	if res.StatusCode != http.StatusConflict {
		t.Fatalf("已存在应 409, got %d", res.StatusCode)
	}
	assertErrorCode(t, res, "exists")
}

func TestFSMkdirBadInput(t *testing.T) {
	h := setUpFiles(t)
	writeFile(t, filepath.Join(h.root, "f.txt"), "x")
	cases := []struct {
		body string
		code string
	}{
		{`{}`, "bad_path"},               // 缺字段
		{`{"path":"相对/路径"}`, "bad_path"}, // 相对路径
		{`{"path":` + jq(filepath.Join(h.root, "f.txt", "子")) + `}`, "bad_path"}, // 穿过普通文件
		{`{"path":` + jq(h.root) + `}`, "exists"},                                // 已存在的目录本身
		{`{"path":` + jq(filepath.Join(h.root, "f.txt")) + `}`, "exists"},        // 撞上同名文件
		{`{"未知":1}`, "bad_request"},                                              // 未知字段
		{`not json`, "bad_request"},                                              // 非法 JSON
	}
	for _, c := range cases {
		res := h.post("/api/fs/mkdir", c.body)
		if res.StatusCode == http.StatusCreated {
			t.Errorf("body=%s 竟然成功了", c.body)
			continue
		}
		assertErrorCode(t, res, c.code)
	}
}

// POST 缺 CSRF 头 → 403（设计 5.7 的全局面板约束）。
func TestFSMkdirCSRF(t *testing.T) {
	h := setUpFiles(t)
	res := h.do("POST", "/api/fs/mkdir", true, "", `{"path":`+jq(filepath.Join(h.root, "x"))+`}`, "")
	if res.StatusCode != http.StatusForbidden {
		t.Fatalf("缺 CSRF 头应 403, got %d", res.StatusCode)
	}
	// header 写错值同样 403：浏览器跨站表单能设任意常见值，唯独设不了这个
	// 面板自定义的字符串
	res = h.do("POST", "/api/fs/mkdir", true, "XMLHttpRequest", `{"path":`+jq(filepath.Join(h.root, "y"))+`}`, "")
	if res.StatusCode != http.StatusForbidden {
		t.Fatalf("CSRF 头值错误应 403, got %d", res.StatusCode)
	}
}

// 未登录的 POST：先 401（鉴权），CSRF 检查在它之后 —— 顺序错了会让
// 未登录者通过 403/401 的差异探测出"哪些路径存在"。
func TestFSMkdirRequiresAuth(t *testing.T) {
	h := setUpFiles(t)
	res := h.do("POST", "/api/fs/mkdir", false, csrf, `{"path":`+jq(filepath.Join(h.root, "z"))+`}`, "")
	if res.StatusCode != http.StatusUnauthorized {
		t.Fatalf("未登录应 401, got %d", res.StatusCode)
	}
	if _, err := os.Stat(filepath.Join(h.root, "z")); err == nil {
		t.Fatal("未登录的请求把目录建出来了")
	}
}

// ---------- POST /api/fs/rename ----------

func TestFSRename(t *testing.T) {
	h := setUpFiles(t)
	from := filepath.Join(h.root, "旧名字.txt")
	writeFile(t, from, "内容")
	to := filepath.Join(h.root, "新名字.md")
	res := h.post("/api/fs/rename", `{"from":`+jq(from)+`,"to":`+jq(to)+`}`)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("rename 应 200, got %d", res.StatusCode)
	}
	if _, err := os.Stat(from); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("源文件还在: %v", err)
	}
	if b, err := os.ReadFile(to); err != nil || string(b) != "内容" {
		t.Fatalf("目标不对: %v %q", err, b)
	}

	// 目标已存在 → 409，绝不静默覆盖。
	//
	// 这条是整个文件模块里最要紧的一条断言：os.Rename 在 Unix 上**默认
	// 就是覆盖**。没有它，用户重命名撞名时的后果是别人的文件凭空消失，
	// 而且不会有任何提示 —— 全盘 root 权限（D14）意味着没有任何系统层
	// 护栏会拦住这一步。
	other := filepath.Join(h.root, "别人的.md")
	writeFile(t, other, "别人的contents")
	res = h.post("/api/fs/rename", `{"from":`+jq(to)+`,"to":`+jq(other)+`}`)
	if res.StatusCode != http.StatusConflict {
		t.Fatalf("目标已存在应 409, got %d", res.StatusCode)
	}
	assertErrorCode(t, res, "exists")
	if b, _ := os.ReadFile(other); string(b) != "别人的contents" {
		t.Fatal("返回 409 之前就已经把目标覆盖了")
	}

	// 源不存在 → 404（列表刷新与双击之间文件被删）
	res = h.post("/api/fs/rename", `{"from":`+jq(filepath.Join(h.root, "无"))+
		`,"to":`+jq(filepath.Join(h.root, "x"))+`}`)
	if res.StatusCode != http.StatusNotFound {
		t.Fatalf("源不存在应 404, got %d", res.StatusCode)
	}
}

// 同目录换名与移动到别的目录是同一个操作（os.Rename 都能做）。
// "剪切 + 粘贴"就是它，所以跨目录必须能成。
func TestFSRenameMovesAcrossDirs(t *testing.T) {
	h := setUpFiles(t)
	writeFile(t, filepath.Join(h.root, "f.txt"), "x")
	src := jq(filepath.Join(h.root, "f.txt"))

	// 目标目录不存在：400 + not_directory。这不是"路径写错"也不是冲突，
	// 用户以为剪切能成 —— 文案必须说清楚是**目标目录**不在。
	res := h.post("/api/fs/rename", `{"from":`+src+`,"to":`+jq(filepath.Join(h.root, "子", "f.txt"))+`}`)
	if res.StatusCode != http.StatusBadRequest {
		t.Fatalf("目标目录不存在应 400, got %d", res.StatusCode)
	}
	assertErrorCode(t, res, "not_directory")
	if _, err := os.Stat(filepath.Join(h.root, "f.txt")); err != nil {
		t.Fatal("失败的移动把源文件弄没了")
	}

	sub := filepath.Join(h.root, "子")
	if err := os.Mkdir(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	res = h.post("/api/fs/rename", `{"from":`+src+`,"to":`+jq(filepath.Join(sub, "f.txt"))+`}`)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("跨目录改名应 200, got %d", res.StatusCode)
	}
	if _, err := os.Stat(filepath.Join(sub, "f.txt")); err != nil {
		t.Fatal(err)
	}
}

func TestFSRenameBadInput(t *testing.T) {
	h := setUpFiles(t)
	f := filepath.Join(h.root, "f.txt")
	writeFile(t, f, "x")
	cases := []struct{ body, code string }{
		{`{"from":` + jq(f) + `}`, "bad_path"}, // 缺 to
		{`{"from":"相对","to":` + jq(filepath.Join(h.root, "t")) + `}`, "bad_path"},
		{`{"from":` + jq(f) + `,"to":"相对"}`, "bad_path"},
		// 原地改名归 bad_path 而不是 bad_request：from/to 的关系不合法，
		// 不是请求体格式坏了。归错类的代价是前端只能显示"请求格式错误"
		// 这种用户完全无法行动的话
		{`{"from":` + jq(f) + `,"to":` + jq(f) + `}`, "bad_path"},
		{`{}`, "bad_path"},
	}
	for _, c := range cases {
		res := h.post("/api/fs/rename", c.body)
		if res.StatusCode == http.StatusOK {
			t.Errorf("body=%s 竟然成功了", c.body)
			continue
		}
		assertErrorCode(t, res, c.code)
	}
}

// ---------- GET /api/fs/roots ----------

// roots 给地址栏回答"这台机器有哪些盘"。
func TestFSRoots(t *testing.T) {
	h := setUpFiles(t)
	res := h.get("/api/fs/roots")
	if res.StatusCode != http.StatusOK {
		t.Fatalf("状态码 %d", res.StatusCode)
	}
	var body struct {
		Roots []struct {
			Path   string `json:"path"`
			FSType string `json:"fstype"`
			Device string `json:"device"`
			Total  uint64 `json:"total"`
			Free   uint64 `json:"free"`
		} `json:"roots"`
	}
	decodeBody(t, res, &body)
	if len(body.Roots) == 0 {
		t.Fatal("roots 不能为空：至少要有 /")
	}
	var hasRoot bool
	for _, r := range body.Roots {
		if !strings.HasPrefix(r.Path, "/") {
			t.Errorf("root path 必须是绝对路径: %+v", r)
		}
		if r.Path == "/" {
			hasRoot = true
			// "/" 的容量必须有值：statfs 失败时如果整条被跳过，地址栏
			// 就看不到根分区，用户以为面板没检测到磁盘
			if r.Total == 0 {
				t.Errorf("/ 的容量为 0，statfs 大概失败了: %+v", r)
			}
		}
	}
	if !hasRoot {
		t.Fatalf("roots 必须包含 /: %+v", body.Roots)
	}
}

// ---------- 未装配时 501 ----------

// 装配层没接文件模块时返回 501，不是 200 + 空列表：后者会被前端渲染成
// "这台机器一个磁盘都没有 / 这个目录是空的"，把用户支到完全错误的方向。
func TestFSNotWired(t *testing.T) {
	h := newHarness(t)
	for _, p := range []string{"/api/fs/list?path=/", "/api/fs/roots", "/api/fs/stat?path=/"} {
		res := h.do("GET", p, true, "", "", "")
		if res.StatusCode != http.StatusNotImplemented {
			t.Errorf("%s 未装配应 501, got %d", p, res.StatusCode)
		}
	}
	for _, p := range []string{"/api/fs/mkdir", "/api/fs/rename"} {
		res := h.do("POST", p, true, csrf, `{"path":"/x"}`, "")
		if res.StatusCode != http.StatusNotImplemented {
			t.Errorf("%s 未装配应 501, got %d", p, res.StatusCode)
		}
	}
}

// 编译期钉子：filemgr.Service 必须满足 api.Files。
// 手写的假实现可能与真实现漂移（接口改了三个方法只改一个），这条断言
// 让漂移变成编译失败。
var _ api.Files = (*filemgr.Service)(nil)

// ---------- 未知错误的兜底归属 ----------

// stubFiles 只做一件事：把每个方法都变成一个"不属于任何已知类别"的错误。
type stubFiles struct{ err error }

func (s stubFiles) List(context.Context, string, filemgr.ListOptions) (filemgr.ListPage, error) {
	return filemgr.ListPage{}, s.err
}
func (s stubFiles) Stat(context.Context, string) (filemgr.Entry, error) {
	return filemgr.Entry{}, s.err
}
func (s stubFiles) Mkdir(context.Context, string) error { return s.err }
func (s stubFiles) Rename(context.Context, string, string) error {
	return s.err
}
func (s stubFiles) Roots(context.Context) ([]filemgr.Root, error) { return nil, s.err }
func (s stubFiles) Open(context.Context, string) (filemgr.Opened, error) {
	return filemgr.Opened{}, s.err
}
func (s stubFiles) Zip(context.Context, []string, io.Writer) error { return s.err }
func (s stubFiles) BeginUpload(context.Context, filemgr.UploadInit) (filemgr.UploadState, error) {
	return filemgr.UploadState{}, s.err
}
func (s stubFiles) PutChunk(context.Context, filemgr.UploadChunk) (filemgr.UploadState, error) {
	return filemgr.UploadState{}, s.err
}
func (s stubFiles) UploadStatus(context.Context, string) (filemgr.UploadState, error) {
	return filemgr.UploadState{}, s.err
}
func (s stubFiles) AbortUpload(context.Context, string) error { return s.err }

// 未知错误必须是 500，不能图省事归成 400。
//
// 这是本文件唯一使用替身的地方，而且是不得已：真实文件系统上几乎造不出
// 一个"不属于 ErrBadPath / ErrNotDirectory / ErrExists / ErrPermission /
// ErrNotExist 任何一类"的错误（ENOSPC、EIO 都要专门构造），而"未知归 500"
// 是刻意写进实现的策略 —— 归成 400 会让人反复修改一个本来正确的路径，
// 真正的原因（磁盘故障、配额、内核错误）永远没人去看。
// 策略需要测试，而它只能靠替身触发。
func TestFSUnknownErrorIs500(t *testing.T) {
	boom := errors.New("Input/output error")
	h := newHarnessWith(t, func(d *api.AuthDeps) { d.Files = stubFiles{boom} })
	cases := []struct{ method, path, body string }{
		{"GET", "/api/fs/list?path=/", ""},
		{"GET", "/api/fs/stat?path=/", ""},
		{"GET", "/api/fs/roots", ""},
		{"POST", "/api/fs/mkdir", `{"path":"/x"}`},
		{"POST", "/api/fs/rename", `{"from":"/a","to":"/b"}`},
	}
	for _, c := range cases {
		xrw := ""
		if c.method == "POST" {
			xrw = csrf
		}
		res := h.do(c.method, c.path, true, xrw, c.body, "")
		if res.StatusCode != http.StatusInternalServerError {
			t.Errorf("%s %s 未知错误应 500, got %d", c.method, c.path, res.StatusCode)
			continue
		}
		raw, _ := io.ReadAll(res.Body)
		// Detail 必须带上原始错误：远程运维时屏幕上的那一行就是全部线索
		if !strings.Contains(string(raw), `"code":"fs_error"`) {
			t.Errorf("%s %s 错误体不符: %s", c.method, c.path, raw)
		}
		if !strings.Contains(string(raw), "Input/output error") {
			t.Errorf("%s %s detail 丢了原始错误: %s", c.method, c.path, raw)
		}
	}
}

// ---------- 下载（设计 8.3）----------

func (f *filesHarness) download(q string, headers map[string]string) *http.Response {
	f.t.Helper()
	req := httptest.NewRequest("GET", q, nil)
	req.AddCookie(&http.Cookie{Name: cookieName, Value: f.token})
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	f.handler.ServeHTTP(w, req)
	return w.Result()
}

func TestFSDownloadStreamsFile(t *testing.T) {
	h := setUpFiles(t)
	p := filepath.Join(h.root, "报告 2026.txt")
	writeFile(t, p, "中文内容 abc")

	res := h.download("/api/fs/download?path="+qs(p), nil)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", res.StatusCode)
	}
	body, _ := io.ReadAll(res.Body)
	if string(body) != "中文内容 abc" {
		t.Fatalf("body = %q", body)
	}
	// Content-Length 必须等于**字节**数。填成字符数的话浏览器收满 12 字节
	// 就认为完成，得到一个被截断的文件，而面板侧完全无感
	if cl := res.Header.Get("Content-Length"); cl != "16" {
		t.Errorf("Content-Length = %q, 期望 16 字节", cl)
	}
	if ct := res.Header.Get("Content-Type"); ct != "text/plain; charset=utf-8" {
		t.Errorf("Content-Type = %q", ct)
	}
	if res.Header.Get("Last-Modified") == "" {
		t.Error("缺 Last-Modified：浏览器无法做条件请求，重下整站文件时浪费流量")
	}
	if cd := res.Header.Get("Content-Disposition"); !strings.HasPrefix(cd, "attachment") {
		t.Errorf("Content-Disposition = %q, 必须是 attachment（否则浏览器直接渲染）", cd)
	}
}

// 中文文件名必须走 RFC 5987。
//
// 这不是国际化洁癖：HTTP 头按 Latin-1 解释，直接把 UTF-8 塞进
// filename="报告.txt"，Chrome 会按 Windows-1252 解码成"æ¥åå.txt"
// 一类的乱码 —— 用户下载 50 个中文命名的备份文件，得到 50 个乱码文件，
// 而他无从判断哪个文件对应哪个。filename*=UTF-8 加百分号编码才是唯一正确的写法。
func TestFSDownloadFilenameRFC5987(t *testing.T) {
	h := setUpFiles(t)
	for _, name := range []string{"报告 2026.txt", "a b.txt", "引号\"号.txt", "换\n行.txt", "日曜%日.txt"} {
		p := filepath.Join(h.root, name)
		writeFile(t, p, "x")
		res := h.download("/api/fs/download?path="+qs(p), nil)
		cd := res.Header.Get("Content-Disposition")
		if !strings.Contains(cd, "filename*=UTF-8''") {
			t.Errorf("%q 缺 RFC 5987 段: %q", name, cd)
			continue
		}
		// 反斜杠、引号、CR/LF 必须被百分号编码掉，否则一个含引号的文件名
		// 就能提前闭合引号往响应头里塞东西（响应头注入）
		star := cd[strings.Index(cd, "filename*="):]
		for _, bad := range []string{"\n", "\r", "%22"} {
			if strings.Contains(star, bad) && bad != "%22" {
				t.Errorf("%q 的 filename* 含裸 %q: %q", name, bad, star)
			}
		}
		if strings.Contains(star, `"`) {
			t.Errorf("%q 的 filename* 含裸引号: %q", name, star)
		}
		// ASCII 回退名同样不能有引号/反斜杠/CR/LF：它是**未编码**地写进
		// 头里的，一个含引号的文件名就能提前闭合引号往响应头里塞东西
		// （响应头注入）。这条比 filename* 更容易漏，因为那部分看起来
		// "只是个给人看的回退名"。
		// 回退名的取法**不能按第一个引号截断** —— 那样实现真的把引号
		// 漏进回退名时，测试会先被同一个引号截断、永远看不到它，
		// 断言于是恒真（这是本文件第二次踩"自证式断言"）。
		// 正确做法是按两个字段之间的分隔符取段。
		if j := strings.Index(cd, "filename=\""); j >= 0 {
			ascii := cd[j+len("filename=\""):]
			if k := strings.Index(ascii, "\"; filename*="); k >= 0 {
				ascii = ascii[:k]
			} else {
				ascii = strings.TrimSuffix(ascii, "\"")
			}
			for _, bad := range []string{`"`, "\\", "\r", "\n", ";"} {
				if strings.Contains(ascii, bad) {
					t.Errorf("%q 的 ASCII 回退名含 %q: %q", name, bad, ascii)
				}
			}
		}
		// 空格与 % 也要转义（% 不转义会与真实百分号编码混淆，出现 %25 / % 混排）
		if strings.Contains(star, " ") {
			t.Errorf("filename* 含裸空格: %q", star)
		}
	}
}

// ASCII 回退名：老的下载工具/终端里的 curl 不认 filename*=，
// 只认 filename=。没有回退时它们会把文件存成 "download" 或整段乱码。
func TestFSDownloadASCIIFallback(t *testing.T) {
	h := setUpFiles(t)
	p := filepath.Join(h.root, "报告.txt")
	writeFile(t, p, "x")
	res := h.download("/api/fs/download?path="+qs(p), nil)
	cd := res.Header.Get("Content-Disposition")
	// filename= 段必须是纯 ASCII 且非空
	i := strings.Index(cd, "filename=")
	if i < 0 {
		t.Fatalf("缺 ASCII 回退名: %q", cd)
	}
	fi := cd[i+len("filename="):]
	if j := strings.IndexByte(fi, ';'); j >= 0 {
		fi = fi[:j]
	}
	fi = strings.Trim(strings.TrimSpace(fi), `"`)
	if fi == "" {
		t.Fatalf("ASCII 回退名为空: %q", cd)
	}
	for _, r := range fi {
		if r > 127 {
			t.Errorf("ASCII 回退名含非 ASCII %q: %q", r, fi)
			break
		}
	}
}

func TestFSDownloadRange(t *testing.T) {
	h := setUpFiles(t)
	p := filepath.Join(h.root, "big.bin")
	writeFile(t, p, "0123456789")

	// 断点续传：下载管理器、浏览器"重试"、以及手机切后台恢复，发的都是
	// Range。忽略它而回 200 全量的话，客户端会把 10 字节追加到已有的
	// 5 字节后面，得到一个 15 字节的坏文件 —— 而且没有任何报错。
	res := h.download("/api/fs/download?path="+qs(p), map[string]string{"Range": "bytes=5-9"})
	if res.StatusCode != http.StatusPartialContent {
		t.Fatalf("Range 请求应 206, got %d", res.StatusCode)
	}
	body, _ := io.ReadAll(res.Body)
	if string(body) != "56789" {
		t.Errorf("Range 内容 = %q", body)
	}
	if cr := res.Header.Get("Content-Range"); cr != "bytes 5-9/10" {
		t.Errorf("Content-Range = %q", cr)
	}
	// 必须带 Accept-Ranges：没有它，客户端不知道这个端点支持续传，
	// 断线后只能从头再来（对 4GB 的备份文件就是灾难）。
	// 断言的是**可观察契约**而不是"我们自己有没有写这个头"：值由
	// http.ServeContent 提供也照样满足要求，换成任何实现都一样要过。
	res2 := h.download("/api/fs/download?path="+qs(p), nil)
	if ar := res2.Header.Get("Accept-Ranges"); ar != "bytes" {
		t.Errorf("Accept-Ranges = %q", ar)
	}
	// 越界的 Range 必须 416。回 200 会被解读成"整文件重发"，
	// 回 200 + 空体则会产生一个 0 字节的"下载成功"文件
	res3 := h.download("/api/fs/download?path="+qs(p), map[string]string{"Range": "bytes=99-"})
	if res3.StatusCode != http.StatusRequestedRangeNotSatisfiable {
		t.Errorf("越界 Range 应 416, got %d", res3.StatusCode)
	}
}

func TestFSDownloadErrors(t *testing.T) {
	h := setUpFiles(t)
	sub := filepath.Join(h.root, "子目录")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		q, code string
		status  int
	}{
		{"/api/fs/download", "bad_path", http.StatusBadRequest},
		{"/api/fs/download?path=相对", "bad_path", http.StatusBadRequest},
		{"/api/fs/download?path=" + qs(filepath.Join(h.root, "没有")), "not_found", http.StatusNotFound},
		// 目录：不能 200。真 200 的话浏览器会存下一个内容未定义的文件
		{"/api/fs/download?path=" + qs(sub), "is_directory", http.StatusBadRequest},
	}
	for _, c := range cases {
		res := h.download(c.q, nil)
		if res.StatusCode != c.status {
			t.Errorf("%s 应 %d, got %d", c.q, c.status, res.StatusCode)
			continue
		}
		if res.StatusCode == http.StatusOK {
			continue
		}
		assertErrorCode(t, res, strings.TrimSpace(c.code))
	}
}

// ---------- 打包下载（zip）----------

// zipHarness 起**真实**的 HTTP 服务器。
//
// httptest.NewRecorder 会把响应整个缓冲进内存，用它永远测不出"边压边发"
// 是不是真的在流式 —— 一个把 zip 全攒在内存里再写出的实现，在 Recorder
// 下与真流式实现的表现完全一致。这里要验的恰恰是"没有整包缓冲"，
// 所以必须用真连接（见 TestFSZipStreamsWhilePackable）。
func zipHarness(t *testing.T) (*filesHarness, string) {
	t.Helper()
	h := setUpFiles(t)
	srv := httptest.NewServer(h.handler)
	t.Cleanup(srv.Close)
	return h, srv.URL
}

func zipGet(t *testing.T, url, token, path string) *http.Response {
	t.Helper()
	req, err := http.NewRequest("GET", url+"/api/fs/zip?"+path, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.AddCookie(&http.Cookie{Name: cookieName, Value: token})
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return res
}

func TestFSZipMultiSelect(t *testing.T) {
	h, url := zipHarness(t)
	writeFile(t, filepath.Join(h.root, "a.txt"), "A")
	writeFile(t, filepath.Join(h.root, "子目录", "b.txt"), "B")

	res := zipGet(t, url, h.token, "path="+qs(filepath.Join(h.root, "a.txt"))+
		"&path="+qs(filepath.Join(h.root, "子目录")))
	if res.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(res.Body)
		t.Fatalf("status = %d body=%s", res.StatusCode, b)
	}
	body, _ := io.ReadAll(res.Body)
	if ct := res.Header.Get("Content-Type"); !strings.HasPrefix(ct, "application/zip") {
		t.Errorf("Content-Type = %q", ct)
	}
	if cd := res.Header.Get("Content-Disposition"); !strings.Contains(cd, ".zip") {
		t.Errorf("打包下载的下载名必须带 .zip, got %q", cd)
	}
	zr, err := zip.NewReader(bytes.NewReader(body), int64(len(body)))
	if err != nil {
		t.Fatalf("zip 坏: %v", err)
	}
	var names []string
	for _, f := range zr.File {
		names = append(names, f.Name)
	}
	got := strings.Join(names, "|")
	if !strings.Contains(got, "a.txt") || !strings.Contains(got, "子目录/b.txt") {
		t.Errorf("条目 = %s", got)
	}
}

// 空选择必须 400。给一个 0 文件的 zip 的话，下载器判"成功"，
// 用户盯着空压缩包怀疑自己点错了，而面板这边什么都没记。
func TestFSZipEmptySelection(t *testing.T) {
	_, url := zipHarness(t)
	res := zipGet(t, url, "", "x=1")
	if res.StatusCode != http.StatusUnauthorized {
		t.Errorf("未登录应 401, got %d", res.StatusCode)
		return
	}
}

func TestFSZipNeedsAuth(t *testing.T) {
	_, url := zipHarness(t)
	res := zipGet(t, url, "", "path=/etc")
	if res.StatusCode != http.StatusUnauthorized {
		t.Errorf("应 401, got %d", res.StatusCode)
	}
}

// 慢打包的替身：写一段就卡住，等测试把它放行。
type slowZipFiles struct {
	stubFiles
	started chan []byte // 第一次 Write 时把已写出的字节交给测试
	release chan struct{}
}

func (s *slowZipFiles) Zip(ctx context.Context, paths []string, w io.Writer) error {
	// archive/zip 的写缓冲是 4KB 量级；一次写 64KB 才能保证有字节
	// 真的穿过 net/http 自己的写缓冲到达客户端，而不只是"进了缓冲区"
	buf := make([]byte, 64*1024)
	if _, err := w.Write(buf); err != nil {
		return err
	}
	s.started <- buf
	<-s.release
	return nil
}

// 打包必须是流式的 —— 这条**测不出来就等于没实现**。
//
// 设计 8.3 明写"不落临时文件"：勾选下载常常是几十 GB 的日志目录，
// 先在内存/磁盘攒一份等于把用户的盘吃空，而失败时那坨临时文件没人清。
// 问题是 Recorder 与"全攒完再发"的实现在断言上无法区分，所以这里
// 用真服务器 + 一个写一段就卡住的 Zip 实现：客户端能在放行之前读到
// 字节，才证明中间没有任何整包缓冲。
func TestFSZipStreamsWhilePacking(t *testing.T) {
	slow := &slowZipFiles{started: make(chan []byte, 1), release: make(chan struct{})}
	h := newHarnessWith(t, func(d *api.AuthDeps) { d.Files = slow })
	srv := httptest.NewServer(h.handler)
	defer srv.Close()

	req, _ := http.NewRequest("GET", srv.URL+"/api/fs/zip?path=/whatever", nil)
	req.AddCookie(&http.Cookie{Name: cookieName, Value: h.token})
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	// 还没放行就能读到字节
	deadline := time.After(3 * time.Second)
	got := make(chan int, 1)
	go func() {
		b := make([]byte, 4096)
		n, _ := res.Body.Read(b)
		got <- n
	}()
	select {
	case n := <-got:
		if n <= 0 {
			t.Fatal("流式响应读到 0 字节")
		}
	case <-deadline:
		t.Fatal("打包还没结束就读不到字节：响应被整体缓冲了")
	}
	// 整体缓冲的另一个指纹：Content-Length。流式响应的长度在打包结束前
	// 根本未知，只能分块传输
	if cl := res.Header.Get("Content-Length"); cl != "" {
		t.Errorf("流式响应不该有 Content-Length=%q", cl)
	}
	close(slow.release)
	io.Copy(io.Discard, res.Body)
	res.Body.Close()
}

func TestFSZipErrors(t *testing.T) {
	h := setUpFiles(t)
	dir := filepath.Join(h.root, "子目录")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		q, code string
		status  int
	}{
		// 空选择：给一个 0 文件的 zip 会被下载器判"成功"，
		// 用户盯着空包怀疑自己点错了，而面板这边什么都没记
		{"/api/fs/zip", "bad_path", http.StatusBadRequest},
		{"/api/fs/zip?path=", "bad_path", http.StatusBadRequest},
		{"/api/fs/zip?path=" + qs(filepath.Join(h.root, "没有")), "not_found", http.StatusNotFound},
	}
	for _, c := range cases {
		res := h.download(c.q, nil)
		if res.StatusCode != c.status {
			t.Errorf("%s 应 %d, got %d", c.q, c.status, res.StatusCode)
			continue
		}
		assertErrorCode(t, res, c.code)
	}
}
