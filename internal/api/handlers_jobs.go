package api

import (
	"context"
	"errors"
	"net/http"
	"strings"

	"litepanel/internal/filemgr"
)

// 后台文件任务的 HTTP 层（设计 709–712 行）。
//
// 这一层的唯一职责是**受理**，不是执行。"受理 ≠ 执行"是 M6-T4 的立足点：
// 处理器必须在任务一个文件都没动的时候就把 job_id 交回去。顺手同步执行
// （旧的 /fs/delete 就是这样）会让 r.Context() 随浏览器关闭而取消，任务
// 就地停在一半 —— 而那恰好是这套队列被造出来要解决的问题。所以配套测试
// 的判据不是"响应里有 job_id"（同步实现也能事后补这个字段），而是"响应
// 回来那一瞬间文件还原地不动"。
//
// 取消端点只写**意图**（cancel_requested），不写终态。终态由拥有该任务的
// 一方落：排队中的由 worker 领取时那一句 UPDATE…CASE 顺手写成 canceled，
// 正在跑的由它的 finalize 写。HTTP 层若图省事直接写 state，就造出两个
// 主人决定"什么时候算取消完了" —— worker 手里还在跑而库里已写 canceled，
// 比不取消更糟。代价是界面上有一瞬显示"排队中 + 已请求取消"，前端按
// cancel_requested 渲染成"正在取消…"，那是真话。

// Jobs 是任务端点需要的能力面。*filemgr.Service 全量满足。
//
// 单独列接口（而不是把方法塞进 Files）是有意的：Files 已经有 15 个方法，
// 而任务端点只用到这 3 个。混在一起的代价是每加一个任务方法，所有 Files
// 的替身夹具都得跟着改，而它们跟任务毫无关系。
type Jobs interface {
	CreateJob(ctx context.Context, in filemgr.JobInput) (filemgr.Job, error)
	ListJobs(ctx context.Context, f filemgr.JobFilter) ([]filemgr.Job, error)
	RequestCancelJob(ctx context.Context, id int64) error
	RetryJob(ctx context.Context, id int64) (filemgr.Job, error)
	// PreflightTrash 在受理删除时检查各盘的回收站建得起来。
	//
	// 它存在的唯一理由是让 ErrTrashUnwritable 继续走 **HTTP 状态码**而不
	// 是退化进任务错误文本：界面上"这个盘只能永久删除"那个按钮靠这个哨兵
	// 才亮得起来，而让前端对中文文案做子串匹配在本项目里是禁止的。
	PreflightTrash(ctx context.Context, paths []string, permanent bool) error
}

var _ Jobs = (*filemgr.Service)(nil)

// handleFSJobSubmit 处理 POST /api/fs/jobs {op,paths[],dst?,permanent?} → 202 {job_id}。
//
// 入参校验的边界是有意的：**只拒"入参本身不合法"**（op 不在封闭集合、
// paths 为空、copy/move 缺 dst），源存在性一律留到执行时。
//
// 不在受理时 stat 每一个源，有两个理由。一是白花一次盘扫描：设计里一次
// 框选可以是几万个路径，为了"提前告诉用户第一个不存在"而扫一遍不值得。
// 更要紧的是**校验结果到执行时已经过期** —— 扫的时候存在，轮到 worker 时
// 可能已经被删了；受理时的"通过"给不了任何保证，却会让人以为有了。
// 执行侧本来就是"先全量校验再动手"，那里的判定才是有效的。
func handleFSJobSubmit(svc Jobs) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			Op        string   `json:"op"`
			Paths     []string `json:"paths"`
			Dst       string   `json:"dst"`
			Permanent bool     `json:"permanent"`
		}
		if err := decodeStrict(r, &in); err != nil {
			writeError(w, http.StatusBadRequest, "bad_request", err.Error())
			return
		}
		// dst 尾部斜杠先剥掉：前端"进入目录后粘贴"很容易交出 /a/b/，而它
		// 会一路拼成 /a/b//name。路径本身合法，但界面上给用户看的目标
		// 出现双斜杠像是拼错了。
		// 删除类任务先做一次回收站预检（理由见 Jobs.PreflightTrash）。
		// 只对 delete 做：copy/move 的目标是用户指定的目录，那里的可写性
		// 由执行时的目标校验负责，与回收站无关。
		if filemgr.JobOp(in.Op) == filemgr.OpDelete {
			if err := svc.PreflightTrash(r.Context(), in.Paths, in.Permanent); err != nil {
				writeJobError(w, err)
				return
			}
		}
		j, err := svc.CreateJob(r.Context(), filemgr.JobInput{
			Op:        filemgr.JobOp(in.Op),
			Src:       in.Paths,
			Dst:       strings.TrimRight(in.Dst, "/"),
			Permanent: in.Permanent,
		})
		if err != nil {
			writeJobError(w, err)
			return
		}
		// 202 而不是 201：201 表示"资源已创建完成"，而这里创建的是一次
		// **尚未开始的**工作。状态码本身就是"受理了、还没做"这句话。
		//
		// 响应里的 job_id 是设计 709 行定的契约名。领域对象自己的字段叫
		// id（它是数据库主键的名字），两者不是一个层面的东西：id 是"这条
		// 记录在哪一行"，job_id 是"你要跟踪的这项工作叫什么"。直接把 Job
		// 整个序列化出去会让前端拿到 id，而它按设计该读 job_id —— 拿到
		// undefined 之后拼出 /fs/jobs/undefined，用户看到的是一次莫名其妙的
		// 404。
		writeJSON(w, http.StatusAccepted, jobResponse{Job: j, JobID: j.ID})
	}
}

// jobResponse 是受理响应：设计 709 行的 {job_id} + 任务全貌（前端建抽屉
// 条目时省一次 GET）。
type jobResponse struct {
	JobID int64 `json:"job_id"`
	filemgr.Job
}

// handleFSJobList 处理 GET /api/fs/jobs?state=。
func handleFSJobList(svc Jobs) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		state := filemgr.JobState(r.URL.Query().Get("state"))
		// 非法 state 明确拒绝，不静默回全部。静默的代价是"筛选看起来能用
		// 而结果永远是全部"：用户在抽屉里勾了"只看失败"而列表一动不动，
		// 他会以为没有失败任务，而实际是筛选从来没生效过。
		if state != "" && !state.Valid() {
			writeError(w, http.StatusBadRequest, "bad_request",
				"state 只能是 pending/running/done/failed/canceled/interrupted")
			return
		}
		jobs, err := svc.ListJobs(r.Context(), filemgr.JobFilter{State: state})
		if err != nil {
			writeJobError(w, err)
			return
		}
		// 空列表必须是 [] 而不是 null：前端 jobs.map 遇 null 白屏。
		// 领域层 ListJobs 成功时恒回非 nil 切片，所以这里不加"兜成 []"的
		// 一层 —— 那会把"实现哪天改成返回 nil"这件事实心实意地藏起来，
		// 而 TestJobsListEmptyIsArray 本该在那时变红。
		writeJSON(w, http.StatusOK, map[string]any{"jobs": jobs})
	}
}

// handleFSJobCancel 处理 DELETE /api/fs/jobs/{id}。
func handleFSJobCancel(svc Jobs) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id, err := chiID(r)
		if err != nil {
			// 400 而不是拿 0 去查库回 404："这个链接无效"与"这条任务不
			// 存在"是两件事。用户从书签或自己写的脚本打过来时，前者才对
			// —— 后者会让人以为任务被人删了。
			writeError(w, http.StatusBadRequest, "bad_request", "任务 id 必须是数字")
			return
		}
		if err := svc.RequestCancelJob(r.Context(), id); err != nil {
			writeJobError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"canceled": true})
	}
}

// writeJobError 把任务层的错误翻成状态码。
//
// 复用 writeFSError 的那套码表（路径类错误两边共用），再叠上任务特有的
// 三种：入参不合法 400、任务不存在 404、已终态不可取消 409。
//
// ErrJobNotCancellable 回 409 而不是 404 是关键的一条：404 的说法是"这条
// 记录从来不存在"，而真相是"它存在且已经结束了"。前端只有拿到 409 才知道
// 该刷新状态而不是把这一行从列表里抹掉。
func writeJobError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, filemgr.ErrJobInput):
		writeError(w, http.StatusBadRequest, "bad_request", err.Error())
	case errors.Is(err, filemgr.ErrNoJob):
		writeError(w, http.StatusNotFound, "not_found", err.Error())
	case errors.Is(err, filemgr.ErrJobNotCancellable):
		writeError(w, http.StatusConflict, "not_cancellable", err.Error())
	case errors.Is(err, filemgr.ErrNoDB):
		// 队列没接数据库是装配问题，不是用户能修的问题 → 503 而不是 500。
		// 500 的说法是"面板坏了"（对，但它说的是"重试也没用"），而这里
		// 重试确实可能成功（面板重启后就好了），前端值得区别对待。
		writeErrorDetail(w, http.StatusServiceUnavailable, "queue_unavailable",
			"后台任务队列不可用", err.Error())
	default:
		writeFSError(w, err)
	}
}

// handleFSJobRetry 处理 POST /api/fs/jobs/{id}/retry（设计 712 行）。
//
// 只有 interrupted 能重试；其余终态与在跑的都回 409（理由见领域层
// RetryJob 与测试）。id 非数字回 400 与取消端点一致。
func handleFSJobRetry(svc Jobs) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id, err := chiID(r)
		if err != nil {
			writeError(w, http.StatusBadRequest, "bad_request", "任务 id 必须是数字")
			return
		}
		j, err := svc.RetryJob(r.Context(), id)
		if err != nil {
			writeJobError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, jobResponse{Job: j, JobID: j.ID})
	}
}
