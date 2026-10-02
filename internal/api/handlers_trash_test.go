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
	svc  *filemgr.Service
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
		// 删除从同步改走队列之后，这个夹具也要带上库与 worker 池：
		// 回收站的行为（进栈/还原/整批拒绝）现在发生在任务执行里，
		// 没有池就没人执行，测试只能验到"受理"。
		svc := filemgr.NewService(filemgr.Options{
			DB:             d.DB,
			ProcDir:        "/proc",
			JobConcurrency: 1,
			FilesystemRoot: func(string) (string, error) { return disk, nil },
			TrashRoots: func(context.Context) ([]string, error) {
				return []string{disk}, nil
			},
		})
		d.Files = svc
		d.Jobs = svc
		h.svc = svc
	})
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	if !h.svc.StartJobs(ctx) {
		t.Fatal("任务池没起来：删除任务的测试全都会挂起")
	}
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

// trash 把一个路径删进回收站并**等任务落地**。
//
// 等是必须的：删除改成队列任务之后，POST 回来时什么都还没发生，而这里的
// 每一个调用方都是"先造一个已在回收站里的条目"这个前置条件 —— 不等就是
// 拿一个还没开始的前置去断言后面的结果，在 CI 上会随机红。
func (h *trashHarness) trash(paths ...string) {
	h.t.Helper()
	var sb strings.Builder
	for i, p := range paths {
		if i > 0 {
			sb.WriteByte(',')
		}
		sb.WriteString(jq(p))
	}
	j := h.deleteJob(h.t, `{"paths":[`+sb.String()+`]}`)
	if j.State != filemgr.JobDone {
		h.t.Fatalf("前置删除该成功, got %s (%s)", j.State, j.Error)
	}
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

// 删除一个文件：受理 202，任务跑完后它进回收站（不是永久删除）。
//
// 200 改 202 是有意的语义变化：受理时**一个文件都还没动**，回 200 +
// "deleted: 1" 会谎称工作已经完成。判据仍然是盘上真相（源消失 + 回收站
// 里有它且能还原），只是要等任务落地之后再验。
func TestDeleteEndpointTrashes(t *testing.T) {
	h := setUpTrash(t)
	p := h.mk(t, "报告.txt", "内容")
	j := h.deleteJob(t, `{"paths":[`+jq(p)+`]}`)
	if j.State != filemgr.JobDone {
		t.Fatalf("任务该成功, got %s (%s)", j.State, j.Error)
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

// 删除一批：一个任务、两条都进回收站，entries_total 记下这批有几条。
//
// "一次框选 500 个文件"必须是**一条**任务而不是 500 条：用户点了一次删除,
// 抽屉里就该只有一条可以取消/重试的记录。
func TestDeleteEndpointBatch(t *testing.T) {
	h := setUpTrash(t)
	a := h.mk(t, "a.txt", "1")
	b := h.mk(t, "b.txt", "2")
	j := h.deleteJob(t, `{"paths":[`+jq(a)+`,`+jq(b)+`]}`)
	if j.State != filemgr.JobDone {
		t.Fatalf("任务该成功, got %s (%s)", j.State, j.Error)
	}
	if j.EntriesTotal != 2 {
		t.Errorf("一批该是一条任务、两条进度, got entries_total=%d", j.EntriesTotal)
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

// paths 里有一个不存在：整批不处理，任务失败，而不是"删了一半"。
//
// 部分成功是最坏的结果：界面上报错，而实际有些文件已经进回收站，用户
// 无法知道到底是哪几个动了。所以实现是"先把整批校验完，再开始动手"。
//
// 同步改异步之后报错时机从 404 变成"一条失败的任务"，但**这条陈诺一字
// 不改**，而且它的价值反而更高了：走队列时提交与执行之间隔得更久，"到
// 底动没动"更需要一个明确答案。判据也从状态码挪到了盘上（回收站里 0 条
// + 那个好文件还在原地），这才是陈诺的本体。
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
			j := h.deleteJob(t, `{"paths":[`+jq(paths[0])+`,`+jq(paths[1])+`]}`)
			if j.State != filemgr.JobFailed {
				t.Fatalf("有一个不存在该让任务失败, got %s", j.State)
			}
			if _, err := os.Lstat(good); err != nil {
				t.Errorf("同批里存在的那个被误删了: %v", err)
			}
			if names := trashNames(t, h.get("/api/fs/trash")); len(names) != 0 {
				t.Errorf("失败的删除批不该留下条目: %v", names)
			}
			// 失败原因要说得出是什么没了：抽屉里一条"失败"而没有对象，
			// 用户只能挨个目录去翻。
			if !strings.Contains(j.Error, "没有.txt") {
				t.Errorf("失败原因该带上那个不存在的路径, got %q", j.Error)
			}
		})
	}
}

// 删除要 CSRF 头（与其它写操作一致）。
//
// 走队列之后这条更该测：缺头时**连任务都不该建出来**。只看"文件还在原地"
// 已经不够了 —— 受理阶段本来就不动文件，任何实现都能过。所以要同时验
// 队列是空的：跨站请求连"排一个删除"都做不到。
func TestDeleteEndpointNeedsCSRF(t *testing.T) {
	h := setUpTrash(t)
	p := h.mk(t, "a.txt", "x")
	res := h.do("POST", "/api/fs/delete", true, "", `{"paths":[`+jq(p)+`]}`, "")
	defer res.Body.Close()
	if res.StatusCode != http.StatusForbidden {
		t.Errorf("缺 CSRF 头该 403, got %d", res.StatusCode)
	}
	if _, err := os.Lstat(p); err != nil {
		t.Error("缺 CSRF 头不该真的删除")
	}
	jobs, err := h.svc.ListJobs(context.Background(), filemgr.JobFilter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(jobs) != 0 {
		t.Errorf("缺 CSRF 头连任务都不该建出来, got %d 条", len(jobs))
	}
}

// permanent=true 永久删除，不进回收站（盘根不可写时这是唯一出路）。
func TestDeleteEndpointPermanent(t *testing.T) {
	h := setUpTrash(t)
	p := h.mk(t, "临时.bin", "x")
	j := h.deleteJob(t, `{"paths":[`+jq(p)+`],"permanent":true}`)
	if j.State != filemgr.JobDone {
		t.Fatalf("永久删除该成功, got %s (%s)", j.State, j.Error)
	}
	if !j.Permanent {
		t.Error("permanent 没传到任务上")
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
	h.trash(p)
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
	h.trash(p)
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
	h.trash(p)
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
	h.trash(h.mk(t, "a.txt", "x"))
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
	h.trash(h.mk(t, "a.txt", "x"), h.mk(t, "b.txt", "y"))
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
		svc := filemgr.NewService(filemgr.Options{
			ProcDir:        "/proc",
			DB:             d.DB, // 受理要落库
			FilesystemRoot: func(string) (string, error) { return disk, nil },
			TrashRoots:     func(context.Context) ([]string, error) { return []string{disk}, nil },
		})
		d.Files = svc
		d.Jobs = svc
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
		// 走队列之后，受理阶段唯一会拿到 context.Canceled 的地方是落库
		// 那一步（CreateJob 用的是 r.Context()），所以替身换成分支上真正
		// 被调用的 Jobs。
		d.Jobs = stubJobs{fmt.Errorf("写入任务: %w", context.Canceled)}
	})
	res := h.do("POST", "/api/fs/delete", true, csrf, `{"paths":["/data/a"]}`, "")
	defer res.Body.Close()
	if res.StatusCode != 499 {
		t.Errorf("客户端断开应 499（与上传同一映射）, got %d", res.StatusCode)
	}
	assertErrorCode(t, res, "canceled")
}

// ---------- 任务化的删除：等待与断言 ----------

// waitJobDone 等一条任务离开活动态，返回它最终的状态。
//
// 删除改成队列任务之后，HTTP 回 202 时**什么还没发生**，所有"文件是不是
// 进了回收站"的断言都必须先等到任务落地。等待而不是 sleep：sleep 要么太短
// （CI 上随机红）要么太长（整套测试慢成几分钟），而"等一个可观察的终态"
// 两头都对。
//
// failed 也算"离开活动态"并原样返回：有一半的用例**要的**就是失败（整批
// 拒绝那条），在助手里 panic 会让那些用例没法写。
func (h *trashHarness) waitJobSettled(t *testing.T, id int64) filemgr.Job {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		j, err := h.svc.GetJob(context.Background(), id)
		if err != nil {
			t.Fatalf("读任务 %d: %v", id, err)
		}
		switch j.State {
		case filemgr.JobDone, filemgr.JobFailed, filemgr.JobCanceled, filemgr.JobInterrupted:
			return j
		}
		time.Sleep(3 * time.Millisecond)
	}
	t.Fatalf("任务 %d 没在 10s 内结束", id)
	return filemgr.Job{}
}

// deleteAndWait 提交删除并等任务落地，回 (响应码, 终态任务)。
//
// 绝大多数删除用例关心的是**结果**，这个助手把"取 job_id + 等收尾"折成一步，
// 免得每个用例都抄两遍而总有一遍抄漏（漏掉等待的用例会在 CI 上随机红，
// 而本地跑得飞快 —— 那是最难查的一类偶发失败）。
func (h *trashHarness) deleteAndWait(t *testing.T, body string) (int, filemgr.Job) {
	t.Helper()
	res := h.postJSON("/api/fs/delete", body)
	if res.StatusCode != http.StatusAccepted {
		defer res.Body.Close()
		return res.StatusCode, filemgr.Job{}
	}
	var b struct {
		JobID int64 `json:"job_id"`
	}
	// decodeBody 会 Close，所以不再另加 defer：同一个 body 关两次不致命，
	// 但会让人以为这里有意为之。
	decodeBody(t, res, &b)
	return http.StatusAccepted, h.waitJobSettled(t, b.JobID)
}

// deleteJob 是"提交删除 + 等落地"的简写，回终态任务。
//
// 名字不说"OK"：它不检查成败，因为有一半用例要的就是一条失败的任务。
func (h *trashHarness) deleteJob(t *testing.T, body string) filemgr.Job {
	t.Helper()
	code, j := h.deleteAndWait(t, body)
	if code != http.StatusAccepted {
		t.Fatalf("受理该 202, got %d", code)
	}
	return j
}

// stubJobs 是只回一个固定错误的 Jobs 替身。
//
// 与 stubFiles 同样的存在理由：只为了验"某个错误映射成哪个状态码"而拉起
// 真库真盘不划算，而这些映射（499/409/503…）是有意写进实现的策略。
type stubJobs struct{ err error }

func (s stubJobs) CreateJob(context.Context, filemgr.JobInput) (filemgr.Job, error) {
	return filemgr.Job{}, s.err
}
func (s stubJobs) ListJobs(context.Context, filemgr.JobFilter) ([]filemgr.Job, error) {
	return nil, s.err
}
func (s stubJobs) RequestCancelJob(context.Context, int64) error        { return s.err }
func (s stubJobs) PreflightTrash(context.Context, []string, bool) error { return s.err }
func (s stubJobs) RetryJob(context.Context, int64) (filemgr.Job, error) {
	return filemgr.Job{}, s.err
}
