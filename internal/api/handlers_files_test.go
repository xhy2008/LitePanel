package api_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

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
