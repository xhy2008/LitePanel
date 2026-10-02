package api_test

// M6-T4：后台文件任务的 HTTP 层（设计 709–712 行的端点表）。
//
// 这一层的测试重点与领域层完全不同，值得先说清楚，否则很容易写成一堆
// "CreateJob 被调到了吗"的重复覆盖。领域层已经证明任务能建、能领、能跑完；
// 这一层要钉的是** HTTP 契约本身 **，其中最重要的一条是：
//
//	受理 ≠ 执行。
//
// 处理器必须在任务**还没开始跑**的时候就把 job_id 交回去。这条看起来稀松
// 平常，却是整个 M6-T4 的立足点：如果处理器顺手同步执行（旧 /fs/delete 就
// 是这样），那么 r.Context() 会随浏览器关闭而取消，任务就地停住 —— 而这
// 恰好是它存在所要解决的问题。判据因此不是"响应里有 job_id"，而是"响应
// 回来时文件还原地不动"：只有真的没同步执行才成立。
//
// 沿用真实 filemgr.Service 而不是替身：任务的错误种类（ErrJobInput 的每条
// 分支、状态机、permanent 落库）只有真库 + 真路径能可靠触发。这里**故意不
// 起 worker 池** —— 队列停在 pending 才是可观察的状态，一旦起了池，"有没
// 有同步执行"就没法区分了（跑太快）。

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"litepanel/internal/api"
	"litepanel/internal/filemgr"
	"litepanel/internal/store"
)

type jobsHarness struct {
	*harness
	svc  *filemgr.Service
	disk string
	db   *store.DB
}

func setUpJobs(t *testing.T) *jobsHarness {
	t.Helper()
	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	disk := filepath.Join(base, "disk")
	if err := os.MkdirAll(disk, 0o755); err != nil {
		t.Fatal(err)
	}
	db, err := store.Open(filepath.Join(base, "jobs.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	h := &jobsHarness{disk: disk, db: db}
	h.harness = newHarnessWith(t, func(d *api.AuthDeps) {
		// 注意**没有** SameFS/JobConcurrency 之类：起了池就没法验"受理时
		// 还没执行"。这里只要库和路径解析。
		svc := filemgr.NewService(filemgr.Options{
			DB:             db,
			ProcDir:        "/proc",
			JobConcurrency: 1,
			FilesystemRoot: func(string) (string, error) { return disk, nil },
			TrashRoots: func(context.Context) ([]string, error) {
				return []string{disk}, nil
			},
		})
		// 两个字段指向同一个对象 —— 生产里也是同一个 *filemgr.Service。
		// 分成两个接口是有意的（zip/upload/trash 的替身夹具不该被迫实现
		// 任务方法），但它们本来就是同一个人的两面，测试照生产装配。
		d.Files = svc
		d.Jobs = svc
		h.svc = svc
	})
	return h
}

func (h *jobsHarness) mk(rel, body string) string {
	h.t.Helper()
	p := filepath.Join(h.disk, rel)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		h.t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		h.t.Fatal(err)
	}
	return p
}

func (h *jobsHarness) post(path, body string) *http.Response {
	h.t.Helper()
	return h.do("POST", path, true, csrf, body, "")
}

func (h *jobsHarness) get(path string) *http.Response {
	h.t.Helper()
	return h.do("GET", path, true, csrf, "", "")
}

func (h *jobsHarness) del(path string) *http.Response {
	h.t.Helper()
	return h.do("DELETE", path, true, csrf, "", "")
}

// jobResp 是 POST /fs/jobs 的响应体。
type jobResp struct {
	ID        int64    `json:"job_id"`
	Op        string   `json:"op"`
	Src       []string `json:"src"`
	Dst       string   `json:"dst"`
	State     string   `json:"state"`
	Permanent bool     `json:"permanent"`
	Error     string   `json:"error"`
	Code      string   `json:"code"`
}

func decodeJob(t *testing.T, res *http.Response) jobResp {
	t.Helper()
	defer res.Body.Close()
	var b jobResp
	if err := json.NewDecoder(res.Body).Decode(&b); err != nil {
		t.Fatalf("解析响应: %v", err)
	}
	return b
}

func (h *jobsHarness) list(t *testing.T) []filemgr.Job {
	t.Helper()
	res := h.get("/api/fs/jobs")
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("列举该 200, got %d", res.StatusCode)
	}
	var b struct {
		Jobs []filemgr.Job `json:"jobs"`
	}
	if err := json.NewDecoder(res.Body).Decode(&b); err != nil {
		t.Fatal(err)
	}
	return b.Jobs
}

// ---------- POST /api/fs/jobs ----------

// 受理必须**立刻**返回 job_id，而任务此时一个文件都还没动。
//
// 这是本文件最重要的一条测试。"响应里带 job_id"本身毫无区分力（同步实现
// 也可以在跑完之后补一个 job_id 字段上去），真正能区分的是**响应回来的
// 那一瞬间文件还在原地**：只有"落库 + 交给后台"才成立。写成同步的后果
// 就是 M6-T4 要解决的那个问题本身 —— 用户关标签页，删除停在一半，而
// 他看到的只是一个网络错误。
func TestJobsSubmitReturnsBeforeDoingAnything(t *testing.T) {
	h := setUpJobs(t)
	p := h.mk("要删的.txt", "内容")
	res := h.post("/api/fs/jobs", `{"op":"delete","paths":[`+jq(p)+`]}`)
	if res.StatusCode != http.StatusAccepted {
		body := decodeJob(t, res)
		t.Fatalf("受理该 202, got %d (%s: %s)", res.StatusCode, body.Code, body.Error)
	}
	b := decodeJob(t, res)
	if b.ID == 0 {
		t.Fatal("响应没带 job_id")
	}
	if _, err := os.Lstat(p); err != nil {
		t.Errorf("受理阶段就动了文件（同步执行了？）: %v", err)
	}
	got := h.list(t)
	if len(got) != 1 {
		t.Fatalf("队列里该有 1 条, got %d", len(got))
	}
	if got[0].State != filemgr.JobPending {
		t.Errorf("刚受理的任务该是 pending, got %s", got[0].State)
	}
}

// 三种 op 都要受理，并且 dst/permanent 按 op 取舍。
//
// delete 带 dst 要**丢掉**而不是报错：前端"剪贴板"里存着目标目录，删除时
// 一并带上来是无害的，为一个不影响语义的字段回 400 只会让用户看到红框。
// copy/move 缺 dst 则必须 400 —— 目标丢了任务根本没法执行，静默接受会让
// 用户以为复制已经开始。
func TestJobsSubmitPerOp(t *testing.T) {
	for _, tc := range []struct {
		name string
		body string
		ok   bool
		code string
	}{
		{"delete 只要 paths", `{"op":"delete","paths":["/x"]}`, true, ""},
		{"delete 带的 dst 被丢弃", `{"op":"delete","paths":["/x"],"dst":"/y"}`, true, ""},
		{"copy 要 dst", `{"op":"copy","paths":["/x"],"dst":"/y"}`, true, ""},
		{"copy 缺 dst 拒绝", `{"op":"copy","paths":["/x"]}`, false, "bad_request"},
		{"move 缺 dst 拒绝", `{"op":"move","paths":["/x"]}`, false, "bad_request"},
		{"未知 op 拒绝", `{"op":"rename","paths":["/x"],"dst":"/y"}`, false, "bad_request"},
		{"空 paths 拒绝", `{"op":"delete","paths":[]}`, false, "bad_request"},
		{"缺 paths 拒绝", `{"op":"delete"}`, false, "bad_request"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := setUpJobs(t)
			res := h.post("/api/fs/jobs", tc.body)
			b := decodeJob(t, res)
			if tc.ok {
				if res.StatusCode != http.StatusAccepted {
					t.Fatalf("该 202, got %d (%s)", res.StatusCode, b.Error)
				}
				return
			}
			if res.StatusCode == http.StatusAccepted {
				t.Fatalf("非法入参该被拒, 却受理了 (job %d)", b.ID)
			}
		})
	}
}

// permanent 必须真的进到任务里去（不能在中途被丢掉）。
//
// 界面上的"永久删除"是二次确认过的不可逆选择。如果 HTTP 层解析了却
// 没往下传，用户勾了"不经过回收站"而实际进了回收站 —— 这个方向看起来
// "更安全"，实际上他可能是**为了不留副本**才选永久删除的（比如里面
// 有密钥）。两个方向都是错，所以要看落库后的值而不是参数名。
func TestJobsSubmitCarriesPermanent(t *testing.T) {
	h := setUpJobs(t)
	p := h.mk("含密钥的日志.txt", "x")
	res := h.post("/api/fs/jobs", `{"op":"delete","paths":[`+jq(p)+`],"permanent":true}`)
	if res.StatusCode != http.StatusAccepted {
		t.Fatalf("该 202, got %d", res.StatusCode)
	}
	b := decodeJob(t, res)
	if !b.Permanent {
		t.Error("响应里的 permanent 丢了")
	}
	got := h.list(t)
	if len(got) != 1 || !got[0].Permanent {
		t.Errorf("permanent 没落库: %+v", got)
	}
}

// 路径写错（不存在）时应当在受理时就拒，而不是建一条注定失败的任务。
//
// 领域层的 delete/copy 是"先全量校验再动手"，而校验发生在 worker 里 ——
// 于是路径写错会变成：界面显示"排队中"→"执行中"→"失败：文件不存在"，
// 用户白等一轮。受理时就拒更好，但**不能**为了这个把整条校验搬到 HTTP
// 层（那会多扫一遍盘，而且校验结果到执行时已经过期）；所以这里采取的
// 是折中：受理时只拒"入参本身不合法"（op/paths/dst），存在性交给执行时
// 的真实校验。本条钉住这个边界，避免后人"顺手加个 stat"。
func TestJobsSubmitDoesNotCheckExistence(t *testing.T) {
	h := setUpJobs(t)
	res := h.post("/api/fs/jobs", `{"op":"delete","paths":[`+jq(filepath.Join(h.disk, "不存在.txt"))+`]}`)
	b := decodeJob(t, res)
	if res.StatusCode != http.StatusAccepted {
		t.Fatalf("不存在的源该照常受理（由执行时报错），got %d: %s", res.StatusCode, b.Error)
	}
}

// 重复源要在受理时就当成一条（而不是让同一个文件被删两次）。
//
// 前端"全选 + 手点两下"很容易交出重复项。领域层 CreateJob 已经去重；
// 这里钉的是 HTTP 层没把它绕过去（比如自己组了个 JobInput 而没走
// CreateJob 的 normalized）。entries_total 是可见证据：它等于去重后的
// 条数，如果绕过了就是 3。
func TestJobsSubmitDedupesSources(t *testing.T) {
	h := setUpJobs(t)
	a := h.mk("a.txt", "1")
	body := fmt.Sprintf(`{"op":"delete","paths":[%s,%s,%s]}`, jq(a), jq(a), jq(a))
	res := h.post("/api/fs/jobs", body)
	if res.StatusCode != http.StatusAccepted {
		t.Fatalf("该 202, got %d", res.StatusCode)
	}
	got := h.list(t)
	if len(got) != 1 {
		t.Fatalf("该只建一条任务, got %d", len(got))
	}
	if got[0].EntriesTotal != 1 {
		t.Errorf("重复源该合成 1 条, got entries_total=%d", got[0].EntriesTotal)
	}
}

// ---------- GET /api/fs/jobs ----------

// 列举必须回数组而不是 null（与 /fs/trash 同一族的前端契约）。
func TestJobsListEmptyIsArray(t *testing.T) {
	h := setUpJobs(t)
	res := h.get("/api/fs/jobs")
	defer res.Body.Close()
	// 用 *[] 反序列化：只有字段是数组（含 []）才会分配出非 nil 指针，
	// null 留 nil。判"能不能 map"这件事只能这样做，decodeBody 分不出
	// null 与 []。
	var b struct {
		Jobs *[]filemgr.Job `json:"jobs"`
	}
	decodeBody(t, res, &b)
	if b.Jobs == nil {
		t.Fatal("jobs 字段必须是数组而不是 null（前端 items.map 遇 null 白屏）")
	}
}

// ?state= 要能筛（任务抽屉默认只看进行中的）。
func TestJobsListStateFilter(t *testing.T) {
	h := setUpJobs(t)
	a := h.mk("a.txt", "1")
	// 一条 pending
	if res := h.post("/api/fs/jobs", `{"op":"delete","paths":[`+jq(a)+`]}`); res.StatusCode != http.StatusAccepted {
		t.Fatalf("该 202, got %d", res.StatusCode)
	}
	res := h.get("/api/fs/jobs?state=pending")
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("该 200, got %d", res.StatusCode)
	}
	var b struct {
		Jobs []filemgr.Job `json:"jobs"`
	}
	decodeBody(t, res, &b)
	if len(b.Jobs) != 1 {
		t.Fatalf("pending 该 1 条, got %d", len(b.Jobs))
	}
	// 一个没有的状态回空数组
	if got := h.getJobsState(t, "done"); len(got) != 0 {
		t.Errorf("done 该 0 条, got %d", len(got))
	}
}

// 非法的 state 值要回 400 而不是静默回全部。
//
// 静默回全部的代价是"筛选看起来能用而结果永远是全部"：抽屉里勾了
// "只看失败"而列表一动不动，用户会以为没有失败任务，而实际上只是筛选
// 从来没生效。状态值是个封闭集合，越界就该明说。
func TestJobsListBadState(t *testing.T) {
	h := setUpJobs(t)
	res := h.get("/api/fs/jobs?state=whatever")
	defer res.Body.Close()
	if res.StatusCode != http.StatusBadRequest {
		t.Errorf("非法 state 该 400, got %d", res.StatusCode)
	}
}

func (h *jobsHarness) getJobsState(t *testing.T, state string) []filemgr.Job {
	t.Helper()
	res := h.get("/api/fs/jobs?state=" + state)
	defer res.Body.Close()
	var b struct {
		Jobs []filemgr.Job `json:"jobs"`
	}
	decodeBody(t, res, &b)
	return b.Jobs
}

// ---------- DELETE /api/fs/jobs/{id} ----------

// 取消一条排队中的任务：200，且一个文件都不许动。
func TestJobsCancelPending(t *testing.T) {
	h := setUpJobs(t)
	p := h.mk("留着.txt", "x")
	res := h.post("/api/fs/jobs", `{"op":"delete","paths":[`+jq(p)+`]}`)
	id := decodeJob(t, res).ID

	cancel := h.del(fmt.Sprintf("/api/fs/jobs/%d", id))
	defer cancel.Body.Close()
	if cancel.StatusCode != http.StatusOK {
		t.Fatalf("取消该 200, got %d", cancel.StatusCode)
	}
	got := h.list(t)
	if len(got) != 1 {
		t.Fatalf("该还是 1 条, got %d", len(got))
	}
	// 断言的是 cancel_requested 而**不是** state==canceled。
	//
	// 这是有意的分工，不是漏实现：取消落库的是"意图"（cancel_requested=1），
	// 终态由**拥有这个任务的一方**去写 —— 排队中的由 worker 领取时那一句
	// UPDATE…CASE 顺手写成 canceled，正在跑的由它的 finalize 写。HTTP 层
	// 若图省事直接写 state='canceled'，就出现了两个主人决定"什么时候算取消
	// 完了"：worker 手里那个任务还在跑，而库里已经写着 canceled（上传那套
	// 设计里同一个"两个主人"的坑，理由完整）。
	//
	// 代价是界面上会有短暂的一瞬显示"排队中 + 已请求取消"，前端拿
	// cancel_requested 渲染成"正在取消…"即可 —— 那是**真话**。
	// 端到端的"canceled 终态 + 文件没动"由 internal/filemgr 的
	// TestDeleteJobCancelBeforeStartTouchesNothing 钉（那里起了真 worker）。
	if !got[0].CancelRequested {
		t.Errorf("取消意图没落库: %+v", got[0])
	}
	if _, err := os.Stat(p); err != nil {
		t.Errorf("被取消的任务动了文件: %v", err)
	}
}

// 取消不存在的任务回 404，取消已结束的回 409。
//
// 两者必须分开：404 的意思是"这个 id 从来不存在"（前端该清掉这条记录），
// 409 的意思是"它存在但已经结束了"（该刷新状态而不是删掉）。合并成一个
// 错误码的话，界面没法决定下一步该做什么。
func TestJobsCancelStatusCodes(t *testing.T) {
	h := setUpJobs(t)
	if res := h.del("/api/fs/jobs/999999"); res.StatusCode != http.StatusNotFound {
		defer res.Body.Close()
		t.Errorf("不存在的任务该 404, got %d", res.StatusCode)
	}
	p := h.mk("x.txt", "x")
	res := h.post("/api/fs/jobs", `{"op":"delete","paths":[`+jq(p)+`]}`)
	id := decodeJob(t, res).ID
	// 手工推到终态（不起池，只能直接改库）
	if _, err := h.db.SqlDB().Exec(`UPDATE fs_jobs SET state='done' WHERE id=?`, id); err != nil {
		t.Fatal(err)
	}
	term := h.del(fmt.Sprintf("/api/fs/jobs/%d", id))
	defer term.Body.Close()
	if term.StatusCode != http.StatusConflict {
		t.Errorf("已结束的任务该 409, got %d", term.StatusCode)
	}
}

// id 必须是数字：非数字回 400，不要拿 0 去查库。
//
// 看着像洁癖，实际区分的是两种完全不同的响应：400 = "你的请求写错了"，
// 404 = "没有这条任务"。用户从书签/脚本里打过来的 URL 属于前者，界面
// 该说的是"链接无效"而不是"任务不存在"（后者会让人以为任务被人删了）。
func TestJobsCancelNonNumericID(t *testing.T) {
	h := setUpJobs(t)
	res := h.del("/api/fs/jobs/abc")
	defer res.Body.Close()
	if res.StatusCode != http.StatusBadRequest {
		t.Errorf("非数字 id 该 400, got %d", res.StatusCode)
	}
}

// ---------- 鉴权与安全面 ----------

// 三个新端点都要吃 CSRF + 登录（它们会改盘上文件）。
//
// 自动纳入现有那张跨端点的安全表（见 handlers_trash_test.go 的同名测试），
// 这里只补"漏了会怎样"的说明：/fs/jobs 能发起删除，一个免 CSRF 的它等于
// 任意站点一张图片就能让用户的面板删文件。
func TestJobsEndpointsRequireCSRF(t *testing.T) {
	h := setUpJobs(t)
	body := `{"op":"delete","paths":["/x"]}`
	// 改状态的两个方法必须吃 CSRF（GET 是安全方法，按设计不吃）。
	// /fs/jobs 能发起删除 —— 一个免 CSRF 的它等于任意网页放一张图片就能
	// 让用户的面板删他自己的文件。
	for _, c := range []struct{ method, path string }{
		{"POST", "/api/fs/jobs"},
		{"DELETE", "/api/fs/jobs/1"},
	} {
		res := h.do(c.method, c.path, true, "", body, "")
		res.Body.Close()
		if res.StatusCode != http.StatusForbidden {
			t.Errorf("%s %s 缺 CSRF 头该 403, got %d", c.method, c.path, res.StatusCode)
		}
	}
	// 三个端点都要登录
	for _, c := range []struct{ method, path string }{
		{"POST", "/api/fs/jobs"},
		{"GET", "/api/fs/jobs"},
		{"DELETE", "/api/fs/jobs/1"},
	} {
		res := h.do(c.method, c.path, false, csrf, body, "")
		res.Body.Close()
		if res.StatusCode != http.StatusUnauthorized {
			t.Errorf("%s %s 未登录该 401, got %d", c.method, c.path, res.StatusCode)
		}
	}
}

// ---------- 未装配 ----------

func TestJobsEndpointsNotWired(t *testing.T) {
	h := newHarnessWith(t, nil)
	for _, r := range []struct{ method, path string }{
		{"POST", "/api/fs/jobs"},
		{"GET", "/api/fs/jobs"},
		{"DELETE", "/api/fs/jobs/1"},
	} {
		res := h.do(r.method, r.path, true, csrf, `{}`, "")
		res.Body.Close()
		if res.StatusCode != http.StatusNotImplemented {
			t.Errorf("%s %s 未装配应 501, got %d", r.method, r.path, res.StatusCode)
		}
	}
}

// 受理一条 delete 任务，返回 job_id（各用例常用的起手式）。
func (h *jobsHarness) submit(t *testing.T, body string) int64 {
	t.Helper()
	res := h.post("/api/fs/jobs", body)
	if res.StatusCode != http.StatusAccepted {
		b := decodeJob(t, res)
		t.Fatalf("提交该 202, got %d: %s", res.StatusCode, b.Error)
	}
	return decodeJob(t, res).ID
}

var _ = time.Second

// ---------- POST /api/fs/jobs/{id}/retry ----------

// 重试一条 interrupted 任务：受理回新状态、任务回到 pending。
func TestJobsRetryInterrupted(t *testing.T) {
	h := setUpJobs(t)
	a := h.mk("x.txt", "1")
	// 建一条任务并手工标成 interrupted（不起池，受理后就停在那）
	res := h.post("/api/fs/jobs", `{"op":"delete","paths":[`+jq(a)+`]}`)
	id := decodeJob(t, res).ID
	if _, err := h.db.SqlDB().Exec(`UPDATE fs_jobs SET state='interrupted' WHERE id=?`, id); err != nil {
		t.Fatal(err)
	}
	rr := h.post(fmt.Sprintf("/api/fs/jobs/%d/retry", id), "")
	defer rr.Body.Close()
	if rr.StatusCode != http.StatusOK {
		t.Fatalf("重试该 200, got %d", rr.StatusCode)
	}
	got := h.list(t)
	for _, j := range got {
		if j.ID == id && j.State != filemgr.JobPending {
			t.Errorf("重试后该回到 pending, got %s", j.State)
		}
	}
}

// 重试非 interrupted 的任务回 409，不存在的回 404。
//
// 与取消端点同一套码：done/canceled 再跑一次只会造出假失败（撞自己的
// 产物），pending/running 再排队会造出同一批文件的第二个执行者。
func TestJobsRetryStatusCodes(t *testing.T) {
	h := setUpJobs(t)
	if res := h.post("/api/fs/jobs/999999/retry", ""); res.StatusCode != http.StatusNotFound {
		defer res.Body.Close()
		t.Errorf("不存在的任务该 404, got %d", res.StatusCode)
	}
	a := h.mk("y.txt", "1")
	res := h.post("/api/fs/jobs", `{"op":"delete","paths":[`+jq(a)+`]}`)
	id := decodeJob(t, res).ID // pending
	term := h.post(fmt.Sprintf("/api/fs/jobs/%d/retry", id), "")
	defer term.Body.Close()
	if term.StatusCode != http.StatusConflict {
		t.Errorf("pending 任务不该能重试, got %d", term.StatusCode)
	}
	if res := h.post("/api/fs/jobs/abc/retry", ""); res.StatusCode != http.StatusBadRequest {
		defer res.Body.Close()
		t.Errorf("非数字 id 该 400, got %d", res.StatusCode)
	}
}

// 重试端点也要 CSRF + 登录。
func TestJobsRetryRequiresCSRF(t *testing.T) {
	h := setUpJobs(t)
	res := h.do("POST", "/api/fs/jobs/1/retry", true, "", "", "")
	defer res.Body.Close()
	if res.StatusCode != http.StatusForbidden {
		t.Errorf("缺 CSRF 头该 403, got %d", res.StatusCode)
	}
	res2 := h.do("POST", "/api/fs/jobs/1/retry", false, csrf, "", "")
	res2.Body.Close()
	if res2.StatusCode != http.StatusUnauthorized {
		t.Errorf("未登录该 401, got %d", res2.StatusCode)
	}
}
