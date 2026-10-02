package filemgr

// 任务状态变更通知（WS fsjobs 频道的领域侧钩子）。
//
// 为什么钩在**写库原语**里，而不是在各个调用点上手写 Broadcast：
// 改动作物的写点有七处（建、领、进度、终态、取消意图、重试、重启对账），
// 手写广播的失效形态是"漏掉一处"，而漏掉的后果不是报错，是**前端任务抽屉
// 停在旧状态默默不再刷新** —— 任务本身跑得好好的，编译通过、测试全绿、
// 日志干净，只有人盯着界面时才暴露。把通知收进写库原语之后，"改了库但没
// 推"需要绕开整个持久层，而那是会编译失败的。
//
// 载荷是 JobProgress 而不是 Job：任务行里存着 src 的**全量路径数组**
// （一键删除十万个文件时可能几 MB），而进度推送的频率上限是每任务每
// 200ms 一次。把整行原样推进 WS，等于在大删除任务进行时每秒往每个订阅
// 连接塞几十 MB，而抽屉只显示一个进度条。字段名与 Job 的 JSON 保持同名
// （有测试钉住），前端才能直接用推送覆盖列表里的那一条。

import (
	"context"
	"time"
)

// JobProgress 是推送给前端的任务摘要：一行任务里**有界**的那部分。
//
// 刻意不含 src。抽屉要显示"正在删除 3/500 项 · 12MB"，这些数字够了；
// 路径列表要看得开 GET /fs/jobs（那是分页的、按需的）。
type JobProgress struct {
	ID              int64    `json:"id"`
	Op              JobOp    `json:"op"`
	Dst             string   `json:"dst"`
	TotalBytes      int64    `json:"total_bytes"`
	DoneBytes       int64    `json:"done_bytes"`
	EntriesTotal    int      `json:"entries_total"`
	EntriesDone     int      `json:"entries_done"`
	State           JobState `json:"state"`
	CancelRequested bool     `json:"cancel_requested"`
	Permanent       bool     `json:"permanent"`
	Resumed         bool     `json:"resumed"`
	Error           string   `json:"error,omitempty"`
	UpdatedAt       int64    `json:"updated_at"`
}

// progressOf 从整行裁出有界摘要。
func progressOf(j Job) JobProgress {
	return JobProgress{
		ID: j.ID, Op: j.Op, Dst: j.Dst,
		TotalBytes: j.TotalBytes, DoneBytes: j.DoneBytes,
		EntriesTotal: j.EntriesTotal, EntriesDone: j.EntriesDone,
		State: j.State, CancelRequested: j.CancelRequested,
		Permanent: j.Permanent, Resumed: j.Resumed,
		Error: j.Error, UpdatedAt: j.UpdatedAt,
	}
}

// notifyJob 重读该行并推一次状态变更。
//
// 为什么重读而不是拿手头的数据拼：七处写点里有一半只握着 id（进度、取消
// 意图），拼出来的对象要靠"我记得刚才写了哪些列"，而漏一列的表现是界面上
// 那个字段永远不动。重读一次让推送的口径**定义上**等于 GET /fs/jobs。
// 代价是每次进度落库多一条 SELECT —— 进度本身已被节流到 200ms/4MB，
// 这条 SELECT 不是瓶颈。
//
// 上下文用 WithoutCancel：取消意图那类写点跑在 HTTP 请求上下文里，用户
// 点完就关标签页时请求上下文已死，而那一瞬间恰恰是任务已经落库、必须让
// 其他页面上的抽屉知道的时刻。超时是防止某个卡住的查询把 worker 拖住。
func (s *Service) notifyJob(ctx context.Context, id int64) {
	if s.jobNotifier == nil {
		return // 没接 WS：不白读一次库
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Second)
	defer cancel()
	j, err := s.GetJob(ctx, id)
	if err != nil {
		// 推不出去只影响界面新鲜度，绝不影响任务本身，所以不向上报错。
		return
	}
	s.jobNotifier(progressOf(j))
}

// notifyJobs 批量推（重启对账用）。
//
// 对账是两条批量 UPDATE，改到的行数等于上次崩溃时在跑 + 在排队被取消的
// 任务数，量级是"并发数 + 排队数"（几十），逐条重读可以接受。
func (s *Service) notifyJobs(ctx context.Context, ids []int64) {
	if s.jobNotifier == nil {
		return
	}
	for _, id := range ids {
		s.notifyJob(ctx, id)
	}
}
