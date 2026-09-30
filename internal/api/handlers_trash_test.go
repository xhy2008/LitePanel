package api_test

// M6-T5：回收站的 HTTP 层（设计 702-705 行 + 删除入口）。
//
// 依旧用**真实的 filemgr.Service**，不用替身：回收站的错误种类
// （ErrTrashUnwritable / ErrExists / ErrBadPath / ErrNotExist）只有真实
// 文件系统 + 真实 rename 能可靠触发（与 handlers_files_test.go 同一个
// 理由）。唯一注入的是两个环境查询：本机的 /proc/mounts 里没有一个真的
// 可写盘能当临时目录的锚点（根是 erofs 只读、/data 根不可写），所以
// 夹具虚构一个盘出来，让删除、列举、还原、清空能在临时目录里真实发生。

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"litepanel/internal/api"
	"litepanel/internal/filemgr"
)

type trashHarness struct {
	*harness
	disk string // 虚构的那个"盘"的根
}

func setUpTrash(t *testing.T) *trashHarness {
	t.Helper()
	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	disk := filepath.Join(base, "disk")
	if err := os.MkdirAll(disk, 0o755); err != nil {
		t.Fatal(err)
	}
	h := &trashHarness{disk: disk}
	h.harness = newHarnessWith(t, func(d *api.AuthDeps) {
		d.Files = filemgr.NewService(filemgr.Options{
			ProcDir:        "/proc",
			FilesystemRoot: func(string) (string, error) { return disk, nil },
			TrashRoots: func(context.Context) ([]string, error) {
				return []string{disk}, nil
			},
		})
	})
	return h
}

// get 带登录态（harness.get 不自动带 cookie，只有 do(...,true,...) 带；
// filesHarness 也是这么包一层的）。
func (h *trashHarness) get(q string) *http.Response {
	h.t.Helper()
	return h.do("GET", q, true, "", "", "")
}

func (h *trashHarness) mk(t *testing.T, rel, body string) string {
	t.Helper()
	p := filepath.Join(h.disk, rel)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func (h *trashHarness) postJSON(path, body string) *http.Response {
	h.t.Helper()
	return h.do("POST", path, true, csrf, body, "")
}

// httpDel 是真正的 HTTP DELETE（用于永久删除条目）。
func (h *trashHarness) httpDel(path string) *http.Response {
	h.t.Helper()
	return h.do("DELETE", path, true, csrf, "", "")
}

// trash 调 POST /api/fs/delete 把一个路径移进回收站（界面上的"删除"）。
func (h *trashHarness) trash(paths ...string) *http.Response {
	h.t.Helper()
	var sb strings.Builder
	for i, p := range paths {
		if i > 0 {
			sb.WriteByte(',')
		}
		sb.WriteString(jq(p))
	}
	return h.postJSON("/api/fs/delete", `{"paths":[`+sb.String()+`]}`)
}

func trashNames(t *testing.T, res *http.Response) []string {
	t.Helper()
	var b struct {
		Items []struct {
			ID     string `json:"id"`
			Name   string `json:"name"`
			Origin string `json:"origin"`
		} `json:"items"`
	}
	decodeBody(t, res, &b)
	out := make([]string, len(b.Items))
	for i, it := range b.Items {
		out[i] = it.Name
	}
	return out
}

// ---------- POST /api/fs/delete ----------

// 删除一个文件要 200 并把它移进回收站（不是永久删除）。
func TestDeleteEndpointTrashes(t *testing.T) {
	h := setUpTrash(t)
	p := h.mk(t, "报告.txt", "内容")
	res := h.postJSON("/api/fs/delete", `{"paths":[`+jq(p)+`]}`)
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("删除应 200, got %d", res.StatusCode)
	}
	if _, err := os.Lstat(p); !os.IsNotExist(err) {
		t.Errorf("源文件还在: %v", err)
	}
	// 现在它必须能被回收站列出来（能还原）
	lst := h.get("/api/fs/trash")
	names := trashNames(t, lst)
	if len(names) != 1 || names[0] != "报告.txt" {
		t.Fatalf("回收站里该有它: %v", names)
	}
}

// 删除一批：每个都进回收站，返回条数。
func TestDeleteEndpointBatch(t *testing.T) {
	h := setUpTrash(t)
	a := h.mk(t, "a.txt", "1")
	b := h.mk(t, "b.txt", "2")
	res := h.postJSON("/api/fs/delete", `{"paths":[`+jq(a)+`,`+jq(b)+`]}`)
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("应 200, got %d", res.StatusCode)
	}
	if names := trashNames(t, h.get("/api/fs/trash")); len(names) != 2 {
		t.Errorf("两条都该进回收站: %v", names)
	}
}

// 空 paths 是 400（前端一个 bug 就可能提交空数组，而这语义模糊）。
func TestDeleteEndpointEmptyPaths(t *testing.T) {
	h := setUpTrash(t)
	res := h.postJSON("/api/fs/delete", `{"paths":[]}`)
	defer res.Body.Close()
	if res.StatusCode != http.StatusBadRequest {
		t.Errorf("空 paths 应 400, got %d", res.StatusCode)
	}
}

// paths 里有一个不存在：整批不处理，回 404，而不是"删了一半"。
//
// 部分成功是最坏的结果：界面上报错，而实际有些文件已经进回收站，用户
// 无法知道到底是哪几个动了。所以实现是"先把整批校验完，再开始动手"。
//
// 两种顺序都要测，而且**好路径在前**那条才是关键：坏路径排第一时，
// 哪怕实现是"边遍历边删"也会在第一步就失败、看起来行为正确；只有坏
// 路径排在后面，"先校验后执行"与"边校验边删"才分道扬镳 —— 前者整批
// 不动，后者已经把前面的删掉了。只测前一种顺序的测试，对这条陈诺
// 什么都没保证。
func TestDeleteEndpointOneMissingRejectsAll(t *testing.T) {
	for _, order := range []struct {
		name  string
		build func(disk string) []string
	}{
		{"坏路径在前", func(disk string) []string {
			return []string{filepath.Join(disk, "没有.txt"), filepath.Join(disk, "good.txt")}
		}},
		{"坏路径在后（先校验的意义所在）", func(disk string) []string {
			return []string{filepath.Join(disk, "good.txt"), filepath.Join(disk, "没有.txt")}
		}},
	} {
		t.Run(order.name, func(t *testing.T) {
			h := setUpTrash(t)
			good := h.mk(t, "good.txt", "x")
			paths := order.build(h.disk)
			res := h.postJSON("/api/fs/delete", `{"paths":[`+jq(paths[0])+`,`+jq(paths[1])+`]}`)
			defer res.Body.Close()
			if res.StatusCode != http.StatusNotFound {
				t.Fatalf("有一个不存在应 404, got %d", res.StatusCode)
			}
			if _, err := os.Lstat(good); err != nil {
				t.Errorf("同批里存在的那个被误删了: %v", err)
			}
			if names := trashNames(t, h.get("/api/fs/trash")); len(names) != 0 {
				t.Errorf("失败的删除批不该留下条目: %v", names)
			}
		})
	}
}

// 删除要 CSRF 头（与其它写操作一致）。
func TestDeleteEndpointNeedsCSRF(t *testing.T) {
	h := setUpTrash(t)
	p := h.mk(t, "a.txt", "x")
	res := h.do("POST", "/api/fs/delete", true, "", `{"paths":[`+jq(p)+`]}`, "")
	defer res.Body.Close()
	// CSRF 缺头的具体码由中间件定（403），这里只确认没被当成合法删除。
	if _, err := os.Lstat(p); err != nil {
		t.Error("缺 CSRF 头不该真的删除")
	}
}

// permanent=true 永久删除，不进回收站（盘根不可写时这是唯一出路）。
func TestDeleteEndpointPermanent(t *testing.T) {
	h := setUpTrash(t)
	p := h.mk(t, "临时.bin", "x")
	res := h.postJSON("/api/fs/delete", `{"paths":[`+jq(p)+`],"permanent":true}`)
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("永久删除应 200, got %d", res.StatusCode)
	}
	if _, err := os.Lstat(p); !os.IsNotExist(err) {
		t.Error("没删掉")
	}
	if names := trashNames(t, h.get("/api/fs/trash")); len(names) != 0 {
		t.Errorf("permanent=true 不该进回收站: %v", names)
	}
}

// ---------- GET /api/fs/trash ----------

func TestTrashListEmptyIsArray(t *testing.T) {
	h := setUpTrash(t)
	res := h.get("/api/fs/trash")
	if res.StatusCode != http.StatusOK {
		defer res.Body.Close()
		t.Fatalf("空回收站应 200, got %d", res.StatusCode)
	}
	body := readBody(t, res) // 自带 Close
	// items 必须是 [] 而不是 null：前端 items.map 遇到 null 直接白屏
	if !strings.Contains(body, `"items":[]`) {
		t.Errorf("空列表该序列化成 []，got %s", body)
	}
}

// 条目要带 enough 字段供界面分组与还原（name/origin/mount/is_dir/size）。
func TestTrashListFields(t *testing.T) {
	h := setUpTrash(t)
	p := h.mk(t, "docs/重要.txt", "内容")
	h.trash(p).Body.Close()
	res := h.get("/api/fs/trash")
	body := readBody(t, res)
	for _, want := range []string{`"name":"重要.txt"`, `"origin":`, `"mount":`, `"is_dir":false`, `"size":`, `"deleted_at":`} {
		if !strings.Contains(body, want) {
			t.Errorf("条目缺字段 %s，got %s", want, body)
		}
	}
}

// ---------- POST /api/fs/trash/{id}/restore ----------

func TestTrashRestoreRoundTrip(t *testing.T) {
	h := setUpTrash(t)
	p := h.mk(t, "报告.txt", "原始内容")
	h.trash(p).Body.Close()
	id := onlyTrashID(t, h.get("/api/fs/trash"))
	res := h.postJSON("/api/fs/trash/"+id+"/restore", `{}`)
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("还原应 200, got %d", res.StatusCode)
	}
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatalf("文件没回到原路径: %v", err)
	}
	if string(b) != "原始内容" {
		t.Errorf("内容变了: %q", b)
	}
	if names := trashNames(t, h.get("/api/fs/trash")); len(names) != 0 {
		t.Errorf("还原后条目该消失: %v", names)
	}
}

// 原路径被占用：还原 409，条目还在回收站。
func TestTrashRestoreConflict(t *testing.T) {
	h := setUpTrash(t)
	p := h.mk(t, "a.txt", "旧")
	h.trash(p).Body.Close()
	id := onlyTrashID(t, h.get("/api/fs/trash"))
	// 原位置占上别的文件
	h.mk(t, "a.txt", "新占位")
	res := h.postJSON("/api/fs/trash/"+id+"/restore", `{}`)
	defer res.Body.Close()
	if res.StatusCode != http.StatusConflict {
		t.Errorf("占用应 409, got %d", res.StatusCode)
	}
	if names := trashNames(t, h.get("/api/fs/trash")); len(names) != 1 {
		t.Errorf("还原失败条目必须还在: %v", names)
	}
}

// 未知 id 回 404。
func TestTrashRestoreUnknownID(t *testing.T) {
	h := setUpTrash(t)
	res := h.postJSON("/api/fs/trash/never-seen/restore", `{}`)
	defer res.Body.Close()
	if res.StatusCode != http.StatusNotFound {
		t.Errorf("未知 id 应 404, got %d", res.StatusCode)
	}
}

// id 带斜杠或点号点号（路径穿越）回 400，不能穿透到盘上。
//
// Chi 会把 /api/fs/trash/a%2Fb/restore 拆开或拒掉；无论哪种，都不能
// 让 ../.. 变成真实的文件操作路径。用 raw 请求确认。
func TestTrashRestoreTraversalID(t *testing.T) {
	h := setUpTrash(t)
	h.mk(t, "victim.txt", "数据纠错")
	for _, id := range []string{"..", "...", "a%2Fb"} {
		res := h.postJSON("/api/fs/trash/"+id+"/restore", `{}`)
		res.Body.Close()
		if res.StatusCode == http.StatusOK {
			t.Errorf("id=%q 不该还原成功", id)
		}
	}
	if _, err := os.Lstat(filepath.Join(h.disk, "victim.txt")); err != nil {
		t.Error("victim 被动了")
	}
}

// ---------- DELETE /api/fs/trash/{id}?confirm=1 ----------

// 永久删除单个条目要带 confirm=1（危险操作，设计 486 行）。
func TestTrashPurgeNeedsConfirm(t *testing.T) {
	h := setUpTrash(t)
	h.trash(h.mk(t, "a.txt", "x")).Body.Close()
	id := onlyTrashID(t, h.get("/api/fs/trash"))
	// 没带 confirm：拒绝，条目还在
	res := h.httpDel("/api/fs/trash/" + id)
	res.Body.Close()
	if res.StatusCode != http.StatusBadRequest {
		t.Errorf("缺 confirm 应 400, got %d", res.StatusCode)
	}
	if names := trashNames(t, h.get("/api/fs/trash")); len(names) != 1 {
		t.Fatalf("缺 confirm 时条目必须还在: %v", names)
	}
	// 带了：删掉
	res = h.httpDel("/api/fs/trash/" + id + "?confirm=1")
	defer res.Body.Close()
	if res.StatusCode != http.StatusNoContent {
		t.Errorf("带 confirm 应 204, got %d", res.StatusCode)
	}
	if names := trashNames(t, h.get("/api/fs/trash")); len(names) != 0 {
		t.Errorf("永久删除后条目该消失: %v", names)
	}
}

func TestTrashPurgeUnknownID(t *testing.T) {
	h := setUpTrash(t)
	res := h.httpDel("/api/fs/trash/never-seen?confirm=1")
	defer res.Body.Close()
	if res.StatusCode != http.StatusNotFound {
		t.Errorf("未知 id 应 404, got %d", res.StatusCode)
	}
}

// ---------- POST /api/fs/trash/empty?confirm=1 ----------

func TestTrashEmptyNeedsConfirm(t *testing.T) {
	h := setUpTrash(t)
	h.trash(h.mk(t, "a.txt", "x"), h.mk(t, "b.txt", "y")).Body.Close()
	res := h.postJSON("/api/fs/trash/empty", `{}`)
	res.Body.Close()
	if res.StatusCode != http.StatusBadRequest {
		t.Errorf("缺 confirm 应 400, got %d", res.StatusCode)
	}
	if names := trashNames(t, h.get("/api/fs/trash")); len(names) != 2 {
		t.Fatalf("缺 confirm 时条目必须都还在: %v", names)
	}
	res = h.postJSON("/api/fs/trash/empty?confirm=1", `{}`)
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Errorf("带 confirm 应 200, got %d", res.StatusCode)
	}
	var b struct {
		Removed int `json:"removed"`
	}
	decodeBody(t, res, &b)
	if b.Removed != 2 {
		t.Errorf("应报 removed:2, got %d", b.Removed)
	}
	if names := trashNames(t, h.get("/api/fs/trash")); len(names) != 0 {
		t.Errorf("清空后不该有残留: %v", names)
	}
}

// ---------- 未装配时 501 ----------

func TestTrashEndpointsNotWired(t *testing.T) {
	h := newHarnessWith(t, nil)
	for _, r := range []struct{ method, path string }{
		{"GET", "/api/fs/trash"},
		{"POST", "/api/fs/trash/x/restore"},
		{"DELETE", "/api/fs/trash/x?confirm=1"},
		{"POST", "/api/fs/trash/empty?confirm=1"},
		{"POST", "/api/fs/delete"},
	} {
		res := h.do(r.method, r.path, true, csrf, `{}`, "")
		res.Body.Close()
		if res.StatusCode != http.StatusNotImplemented {
			t.Errorf("%s %s 未装配应 501, got %d", r.method, r.path, res.StatusCode)
		}
	}
}

// ---------- 助手 ----------

func (h *trashHarness) root(rel string) string { return filepath.Join(h.disk, rel) }

func onlyTrashID(t *testing.T, res *http.Response) string {
	t.Helper()
	defer res.Body.Close()
	var b struct {
		Items []struct {
			ID string `json:"id"`
		} `json:"items"`
	}
	decodeBody(t, res, &b)
	if len(b.Items) != 1 {
		t.Fatalf("回收站应恰好 1 条, got %d", len(b.Items))
	}
	return b.Items[0].ID
}

var _ = time.Second // 预留给将来的定时清理端点测试

// ---------- 盘根不可写时的状态码 ----------

// 盘根写不进去（回收站建不起来）必须是可区分的错误码，不能是 500。
//
// 默认映射会把 ErrTrashUnwritable 兜成 500 "文件系统操作失败"：那是
// "面板出 bug 了"的意思，而实际含义是"这个盘不让写，你只能永久删除"。
// 前端要靠这个 code 把"永久删除（二次确认）"这个替代动作摆出来 —— 没有
// 专属 code，用户只会看到一个红色 500 而不知道下一步该做什么。
//
// 触发方式用真实文件系统：把盘根 chmod 0500，删除其子目录里的文件，
// 于是"移出"可以而"建回收站"必然 EACCES（本机实测的权限模型）。
func TestDeleteTrashUnwritableMapsToCode(t *testing.T) {
	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	disk := filepath.Join(base, "disk")
	sub := filepath.Join(disk, "sub")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(sub, "重要.bin")
	if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	h := newHarnessWith(t, func(d *api.AuthDeps) {
		d.Files = filemgr.NewService(filemgr.Options{
			ProcDir:        "/proc",
			FilesystemRoot: func(string) (string, error) { return disk, nil },
			TrashRoots:     func(context.Context) ([]string, error) { return []string{disk}, nil },
		})
	})
	// 盘根只读：建 .trash 必然失败
	if err := os.Chmod(disk, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(disk, 0o755) })

	res := h.do("POST", "/api/fs/delete", true, csrf, `{"paths":[`+jq(p)+`]}`, "")
	defer res.Body.Close()
	if res.StatusCode == http.StatusInternalServerError {
		t.Errorf("盘根不可写不该报 500（那是「面板坏了」的意思）, got %d", res.StatusCode)
	}
	assertErrorCode(t, res, "trash_unwritable")
	if _, err := os.Lstat(p); err != nil {
		t.Error("失败时源文件必须完好")
	}
}

// 客户端断开（关标签页/锁屏）不该被报成"面板坏了"。
//
// 批量删除跑到一半用户关掉浏览器是常态，而 r.Context() 随之取消，DOMAIN
// 层回 context.Canceled —— 没映射就是 500 "文件系统操作失败"。上传那边
// 早就映射了 499（同 handlers_upload.go），文件操作这边漏了：面板没坏，
// 是客户端自己走的，这个区分是反代日志与告警唯一能看出真相的地方。
func TestDeleteCanceledMapsToClientGone(t *testing.T) {
	h := newHarnessWith(t, func(d *api.AuthDeps) {
		d.Files = stubFiles{fmt.Errorf("移入回收站: %w", context.Canceled)}
	})
	res := h.do("POST", "/api/fs/delete", true, csrf, `{"paths":["/data/a"]}`, "")
	defer res.Body.Close()
	if res.StatusCode != 499 {
		t.Errorf("客户端断开应 499（与上传同一映射）, got %d", res.StatusCode)
	}
	assertErrorCode(t, res, "canceled")
}
