package download

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Service 把两个真相来源合成一份前端可用的下载列表。
//
// 分工（写死在这里，因为它决定了每条合成规则）：
//
//   - **进行中**（active / waiting、速度、已下字节）归 aria2 独占。面板不往
//     库里镜像这些值（见 tasks.go 顶部）：每 tick 写一次 SQLite 会直接违背
//     本里程碑"面板开销极低"的承诺，而且会造出一个必然与 aria2 漂移的副本
//     —— aria2 侧还能被别的客户端（Web UI、aria2rpc）改动。
//   - **提交时的事实**（name/dir/uris）与**终态**（complete/error/removed +
//     原因 + 完成时的大小）归本地表。终态归本地不是优化而是**必需**：实测
//     aria2 重启后 tellStopped 一条终态记录都不留。
//
// 于是每个视图都是合成的，两条规则各有一条测试：
//  1. 同一个 gid 两边都有时只出一条（完成事件先落库、aria2 下一轮才把它从
//     tellActive 摘掉，这个窗口是常态）；
//  2. 本地已是终态的，不被 aria2 的 active 倒回去（否则卡片在"完成/下载中"
//     之间来回抖）。

// MaxSplit 是分片数的上限。
//
// 夹住而不是原样转发：aria2 对 split 不做上界校验，给它 1e9 不会报错，而是按
// 用户输入去开连接、分配缓冲 —— 这是一个能从网页发起的资源放大面。夹住之后
// 最坏情况是"实际分片比用户填的少"，不伤机器。取值参考 aria2 自身默认
// （max-concurrent-downloads 与 split 的量级），再高在 HDD 上也不换收益。
const MaxSplit = 64

// aria2 是一个能力面（生产由 *Client 满足）。
//
// 单独列接口不是为了"可替换"，而是让 handler 层能不起 aria2 进程就测完状态码
// 映射 —— 而"aria2 没起来"这一类恰恰没法用一个起着的 aria2 来测。
type aria2 interface {
	TellActive(ctx context.Context) ([]Status, error)
	TellWaiting(ctx context.Context) ([]Status, error)
	TellStatus(ctx context.Context, gid string) (*Status, error)
	GetGlobalStat(ctx context.Context) (*GlobalStat, error)
	AddURI(ctx context.Context, uris []string, o Options) (string, error)
	Pause(ctx context.Context, gid string) error
	Resume(ctx context.Context, gid string) error
	Remove(ctx context.Context, gid string) error
	ForceRemove(ctx context.Context, gid string) error
	GetVersion(ctx context.Context) (string, error)
}

// TaskView 是前端下载页的一行。
//
// 数字全是 JSON number 而不是 aria2 的字符串：转换在边界做一次。留给前端
// Number() 的话，每个消费点都要写一遍，漏一处就是 "250/1000" 这种字符串
// 拼出来的百分比。
type TaskView struct {
	GID         string   `json:"gid"`
	URIs        []string `json:"uris"`
	Name        string   `json:"name,omitempty"`
	Dir         string   `json:"dir,omitempty"`
	State       string   `json:"state"`
	Error       string   `json:"error,omitempty"`
	TotalBytes  int64    `json:"total_bytes"`
	DoneBytes   int64    `json:"done_bytes"`
	Speed       int64    `json:"speed"`
	Connections int      `json:"connections"`
	CreatedAt   int64    `json:"created_at"`
	FinishedAt  int64    `json:"finished_at,omitempty"`
	// CanControl 报告这条任务面板能不能真的操作。
	//
	// 别人（aria2 的 Web UI、脚本）提交的任务面板也要**显示**，但它不在本地表
	// 里，历史与 name 都无从谈起；给前端一个明确的标志，比让它对 name=="" 做
	// 猜测稳。
	CanControl bool `json:"can_control"`
}

// Summary 是下载页顶部那条汇总。
type Summary struct {
	Speed   int64 `json:"speed"`
	Active  int   `json:"active"`
	Waiting int   `json:"waiting"`
	Paused  int   `json:"paused"`
	Done    int   `json:"done"`
	Failed  int   `json:"failed"`
}

// AddInput 是新建下载的入参（handler 已做完协议与目录校验）。
type AddInput struct {
	URIs  []string
	Dir   string
	Out   string
	Split int
}

// ServiceOptions 是 Service 的可选参数。
type ServiceOptions struct {
	// HealthTTL 是探活结果的缓存时长；0 用 HealthChecker 的默认值。
	HealthTTL time.Duration
	// WSURL 是 aria2 的事件流地址（ws://…/jsonrpc）。空 = 不起事件桥，
	// 状态变化只靠轮询器发现（进度本来就靠轮询，事件只是让它更及时）。
	WSURL string
	// PollInterval 是进度轮询间隔；0 用 Poller 的默认值（1s）。
	PollInterval time.Duration
}

// Service 是下载模块对外的门面。可并发使用。
type Service struct {
	a2    aria2
	tasks *TaskStore
	probe *HealthChecker
	opt   ServiceOptions

	// OnNeedPoll 在"有新的可轮询任务"时被调用，用来叫醒轮询器。
	//
	// 生产里由 Run 接到位（Service 自己持有轮询器时直接调 Wake）；留着这个
	// 钩子是为了让没起 Run 的场合（单元测试、只读端点）也能表达"该轮了"。
	OnNeedPoll func()

	// OnEvent 收到每一条下载状态变化（事件桥或轮询器产生的都算）。
	//
	// 顺序契约：Service 先调 OnEvent、**它返回之后**才把事件推给前端
	// （ViaWS）。装配层因此可以把"落库"放进来，前端随后拉列表时必然读到新
	// 状态。反过来（先推后落）会闪一帧旧状态 —— 界面上"完成"跳回"下载中"
	// 再变"完成"，看起来像面板在抽风。
	OnEvent func(Event)

	// ViaWS 由装配层接上（api.BroadcastDownload(hub)），收到与历史事件**同
	// 一条**推送。它可以由多个来源调用（事件桥、轮询器），去重不在这里做
	// —— 前端把任意多条事件合并成一次列表刷新，多发一条只是多一次 GET。
	ViaWS func(Event)

	// add 是"新任务的面板侧默认"（下载目录 / 分片数），设置页可热改。
	addDefaults atomic.Pointer[addDefaults]

	mu       sync.Mutex
	progress map[string]Progress
	poller   *Poller
}

// addDefaults 是新建下载任务时注入的面板默认值。
type addDefaults struct {
	dir   string
	split int
}

// NewService 装配下载模块。
func NewService(a2 aria2, tasks *TaskStore, opt ServiceOptions) *Service {
	s := &Service{a2: a2, tasks: tasks, progress: map[string]Progress{}, opt: opt}
	s.addDefaults.Store(&addDefaults{}) // 非 nil 初值：读侧不必每处判空
	s.probe = NewHealthChecker(a2, opt.HealthTTL)
	s.poller = NewPoller(
		func(ctx context.Context) ([]Status, error) { return a2.TellActive(ctx) },
		func(ctx context.Context, gid string) (*Status, error) { return a2.TellStatus(ctx, gid) },
		s.emit,
		PollerOptions{
			Interval:   opt.PollInterval,
			OnProgress: s.SetProgress,
		},
	)
	return s
}

// emit 是轮询器/事件桥的统一出口：先 OnEvent（落库），后 ViaWS（推前端）。
//
// 顺序写死在这里而不是交给调用方，是因为这条线有两个生产者（事件桥与轮询器）：
// 交给调用方保证意味着两处都要保证，而漏一处的表现（界面闪旧状态）只在真机
// 上偶发，测试极难抓。
func (s *Service) emit(ev Event) {
	if s.OnEvent != nil {
		s.OnEvent(ev)
	}
	if s.ViaWS != nil {
		s.ViaWS(ev)
	}
}

// Run 起事件桥与进度轮询器（各自的 goroutine），ctx 取消时收干净。
//
// 事件桥起不起来取决于 WSURL 配了没有：没配也要能跑（进度靠轮询），只是
// 状态变化的及时性下降 —— 这是可选增强，不是硬依赖。aria2 没起时两个循环
// 都会自己退避重连（事件桥内置）/ 不启动（轮询器空转即停），不需要这里
// 特殊处理。
func (s *Service) Run(ctx context.Context) {
	if s.poller != nil {
		s.poller.Wake()
		go s.poller.Run(ctx)
		go func() {
			<-ctx.Done()
			s.poller.Close()
		}()
	}
	if s.opt.WSURL != "" {
		br := NewEventBridge(s.opt.WSURL, s.emit, EventBridgeOptions{
			FullSync: func(ctx context.Context) ([]Status, error) {
				return s.liveStatuses(ctx)
			},
			Lookup: func(ctx context.Context, gid string) (*Status, error) {
				return s.a2.TellStatus(ctx, gid)
			},
		})
		go br.Run(ctx)
		go func() {
			<-ctx.Done()
			br.Close()
		}()
	}
}

// Health 报告 aria2 状态。它自己不返回 error：见 HealthChecker.Check 的理由
// （"aria2 没起来"是数据，不是服务器错误）。
func (s *Service) Health(ctx context.Context) Health {
	return *s.probe.Check(ctx)
}

// ForgetHealth 让"设置页刚改了 RPC 地址"能立刻重探。
func (s *Service) ForgetHealth() { s.probe.Forget() }

// SetProgress 收下轮询器本轮的进度快照。
//
// 与 TaskStore.ApplyProgress 同源（同一份数据两处要：内存里贴给视图、以及
// 供未来别的消费方读）。这里再存一份是为了让 Service 不依赖 TaskStore 的内部
// 节奏，代价是一次 map 拷贝 —— 每次 tick 几十条，可忽略。
func (s *Service) SetProgress(items []Progress) {
	s.tasks.ApplyProgress(items)
	next := make(map[string]Progress, len(items))
	for _, p := range items {
		next[p.GID] = p
	}
	s.mu.Lock()
	s.progress = next
	s.mu.Unlock()
}

func (s *Service) prog(gid string) (Progress, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	p, ok := s.progress[gid]
	return p, ok
}

// Tasks 合成完整列表：进行中的来自 aria2，终态的来自本地表。
func (s *Service) Tasks(ctx context.Context) ([]TaskView, error) {
	live, err := s.liveStatuses(ctx)
	if err != nil {
		// 不做"降级成只显示历史"的兜底：那会让界面显示"没有任何任务在下",
		// 而真相是"aria2 没起来"。用户于是去点新建（然后失败），而安装引导
		// 永远出不来。错误原样上抛，handler 才能回 503。
		return nil, err
	}
	records, err := s.tasks.List(ctx, ListFilter{})
	if err != nil {
		return nil, err
	}

	byGID := make(map[string]Record, len(records))
	for _, r := range records {
		byGID[r.GID] = r
	}

	var out []TaskView
	seen := map[string]bool{}
	now := time.Now().Unix()

	// 1) 进行中的：以 aria2 为准，本地记录补 name/dir/created_at。
	for _, st := range live {
		view := viewFromStatus(st, now)
		if rec, ok := byGID[st.GID]; ok {
			if rec.State.IsTerminal() {
				// 规则 2：本地已是终态，aria2 的 active 不能把它倒回去。
				view = viewFromRecord(rec)
			} else {
				mergeRecord(&view, rec)
			}
		}
		s.applyProgress(&view)
		out = append(out, view)
		seen[st.GID] = true
	}

	// 2) 本地记录：终态的一律要显示（这是本地表存在的全部理由）；未终结但
	//    aria2 没列出来的也要显示（刚提交还没轮到、或 aria2 已经忘了它），
	//    否则用户会以为"我的下载不见了"。
	for _, rec := range records {
		if seen[rec.GID] {
			continue
		}
		view := viewFromRecord(rec)
		s.applyProgress(&view)
		out = append(out, view)
		seen[rec.GID] = true
	}

	sortViews(out)
	if out == nil {
		// 空列表必须是空切片：nil marshal 成 null，前端 data.tasks.length
		// 直接抛 TypeError，整个下载页白屏。
		out = []TaskView{}
	}
	s.wakeIfPollable(live, records)
	return out, nil
}

// liveStatuses 拿 aria2 侧全部"可轮询"的任务（active + waiting）。
//
// 两次调用而不是一个方法：aria2 没有"给我所有未结束任务"的单一调用，
// tellWaiting 不含 active，而 paused 的没有进度可轮（速度恒 0）。
func (s *Service) liveStatuses(ctx context.Context) ([]Status, error) {
	active, err := s.a2.TellActive(ctx)
	if err != nil {
		return nil, err
	}
	waiting, err := s.a2.TellWaiting(ctx)
	if err != nil {
		return nil, err
	}
	return append(active, waiting...), nil
}

// wakeIfPollable 在"有得可轮"时叫醒轮询器。
func (s *Service) wakeIfPollable(live []Status, records []Record) {
	if s.OnNeedPoll == nil && s.poller == nil {
		return
	}
	if len(live) > 0 {
		s.wake()
		return
	}
	for _, r := range records {
		if !r.State.IsTerminal() {
			s.wake()
			return
		}
	}
}

// wake 叫醒轮询器：优先走注入的 OnNeedPoll（测试注入它来断言"该轮了"），
// 否则直接叫自己持有的轮询器。
func (s *Service) wake() {
	if s.OnNeedPoll != nil {
		s.OnNeedPoll()
		return
	}
	if s.poller != nil {
		s.poller.Wake()
	}
}

// applyProgress 把轮询器最新一轮的数字贴到视图上。
//
// 只覆盖非零值：aria2 的 tellActive 与轮询的 tellActive 是同一份数据的不同
// 时刻，轮询那轮可能因为任务刚建好而回空串（→ 0），此时保留 aria2 那次
// 查询拿到的值更准。
func (s *Service) applyProgress(v *TaskView) {
	// 终态行**完全不贴**快照：库里存的是完成那一刻的最终大小，而快照停在
	// 完成前的最后一轮（aria2 一摘掉任务，轮询器就再也刷新不到它）。无条件
	// 覆盖会做出"已完成 · 33%"这种自相矛盾的行 —— 真机实测到过。
	// 顺带也保证了终态行的 speed 恒为 0：没在下的任务显示一个速度更难解释。
	if State(v.State).IsTerminal() {
		return
	}
	p, ok := s.prog(v.GID)
	if !ok {
		return
	}
	if p.TotalBytes != 0 {
		v.TotalBytes = p.TotalBytes
	}
	if p.DoneBytes != 0 {
		v.DoneBytes = p.DoneBytes
	}
	if p.Speed != 0 {
		v.Speed = p.Speed
	}
	if p.Connections != 0 {
		v.Connections = p.Connections
	}
}

func viewFromStatus(st Status, now int64) TaskView {
	return TaskView{
		GID: st.GID, State: st.Status,
		TotalBytes:  a2int(st.TotalLength),
		DoneBytes:   a2int(st.CompletedLength),
		Speed:       a2int(st.DownloadSpeed),
		Connections: int(a2int(st.Connections)),
		CreatedAt:   now,
		CanControl:  true,
	}
}

func viewFromRecord(r Record) TaskView {
	return TaskView{
		GID: r.GID, URIs: r.URIs, Name: r.Name, Dir: r.Dir,
		State: string(r.State), Error: r.Error,
		TotalBytes: r.TotalBytes, DoneBytes: r.CompletedBytes,
		CreatedAt: r.CreatedAt, FinishedAt: r.FinishedAt,
		CanControl: true,
	}
}

// mergeRecord 用本地记录补 aria2 没有的字段。
//
// name/dir 必须从本地取：aria2 的 tellActive **不返回**这两个字段（要
// tellStatus 逐个问，而"每 tick 逐任务问一次"正是本模块要避免的开销模式）。
func mergeRecord(v *TaskView, r Record) {
	if v.Name == "" {
		v.Name = r.Name
	}
	if v.Dir == "" {
		v.Dir = r.Dir
	}
	if len(v.URIs) == 0 {
		v.URIs = r.URIs
	}
	if r.CreatedAt != 0 {
		v.CreatedAt = r.CreatedAt
	}
}

// sortViews 新提交在前。
//
// 次序键用 CreatedAt 而不是 id：外部任务（面板没提交过的）没有 id。同一时刻
// 提交的（并发提交、或测试里的固定时钟）再按 gid 排，保证结果稳定 —— 一次
// 不稳定的排序在界面上表现为列表每次刷新都重排一次。
func sortViews(v []TaskView) {
	sort.SliceStable(v, func(i, j int) bool {
		if v[i].CreatedAt != v[j].CreatedAt {
			return v[i].CreatedAt > v[j].CreatedAt
		}
		return v[i].GID > v[j].GID
	})
}

// Summary 合成顶部汇总条。
//
// 数量分两头取是必须的：aria2 的 numStopped 跨重启归零（实测），用它显示
// "已完成 40 个"会在每次 aria2 重启后变成 0，界面看起来像历史被清了。
func (s *Service) Summary(ctx context.Context) (Summary, error) {
	var out Summary
	live, err := s.liveStatuses(ctx)
	if err != nil {
		return out, err
	}
	for _, st := range live {
		switch st.Status {
		case "active":
			out.Active++
		case "waiting":
			out.Waiting++
		case "paused":
			out.Paused++
		}
	}
	if g, err := s.a2.GetGlobalStat(ctx); err == nil {
		out.Speed = a2int(g.DownloadSpeed)
	}
	counts, err := s.tasks.CountByState(ctx)
	if err != nil {
		return out, err
	}
	out.Done = counts[StateComplete]
	out.Failed = counts[StateError]
	if out.Paused == 0 {
		// aria2 的 tellWaiting 不含 paused，暂停中的任务只存在于本地状态里。
		out.Paused = counts[StatePaused]
	}
	return out, nil
}

// Add 提交一个下载：**先问 aria2，成功后才落库**。
//
// 顺序反过来（先落库）会在 aria2 拒绝时留下一条永远不动的幽灵记录：历史里
// 挂着一条 active、进度永远 0、aria2 那边根本没这个 gid，而且它还会在下次
// 启动对账时被收口成"失败" —— 用户在历史里看到一条自己从没成功提交过的下载。
// SetDefaults 设新任务的面板默认（下载目录 / 分片数），设置页热生效。
//
// 只影响之后新建的任务：dir 与 split 都是 aria2 的**每任务**选项，已在
// 下载的任务改不了（aria2 没有"把这个进行中的任务挪到别的目录"这种操作）。
// 界面上要说清"对进行中的任务无效"，不能让用户以为改了所有任务就搬家了。
func (s *Service) SetDefaults(dir string, split int) {
	if split < 0 {
		split = 0
	}
	if split > MaxSplit {
		split = MaxSplit
	}
	s.addDefaults.Store(&addDefaults{dir: dir, split: split})
}

func (s *Service) Add(ctx context.Context, in AddInput) (TaskView, error) {
	var zero TaskView
	if len(in.URIs) == 0 {
		return zero, errors.New("下载地址不能为空")
	}
	// 用户没指定时用设置页的面板默认。
	//
	// 为什么要在**这里**填而不是让 aria2 的全局默认兜住：面板自己那张任务表
	// 也记着 dir，列表要显示"下载到哪"。如果只在 aria2 侧设全局默认而本地
	// 记空串，界面上每条任务的目的地都是空的 —— 用户看不到文件落在哪，
	// 而"文件在哪"是下载页最需要回答的问题。
	// 不判 nil：NewService 存了非 nil 初值，SetDefaults 也只存非 nil 指针，
	// "恒非 nil"是构造期建立的不变式。判空会把它的破坏静默降级成"默认值
	// 悄悄变成零"，而零目录意味着文件下到 aria2 自己的当前目录——比崩溃更难查。
	d := s.addDefaults.Load()
	dir, split := in.Dir, in.Split
	if dir == "" {
		dir = d.dir
	}
	if split == 0 {
		split = d.split
	}
	if split > MaxSplit {
		split = MaxSplit
	}
	if split < 0 {
		split = 0
	}
	gid, err := s.a2.AddURI(ctx, in.URIs, Options{Dir: dir, Out: in.Out, Split: split})
	if err != nil {
		return zero, err
	}
	rec, err := s.tasks.Add(ctx, Submission{URIs: in.URIs, GID: gid, Dir: dir, Name: in.Out})
	if err != nil {
		// aria2 已经收下而本地没记下：任务会正常下载，只是不在面板历史里。
		// 报错但不回滚（强撤一个已经在下载的任务更糟）；错误原文带上前因，
		// 用户至少知道文件其实在下。
		return zero, fmt.Errorf(" aria2 已受理但面板没能记下这条任务（文件仍会正常下载）: %w", err)
	}
	view := viewFromRecord(*rec)
	view.State = string(StateActive)
	s.wake()
	return view, nil
}

// known 报告这个 gid 是不是面板或 aria2 认识的任务。
//
// 用它把"引用失效"（404，前端该刷新列表）与"aria2 拒绝了这个操作"（4xx +
// 原文）分开，而不是靠解析 aria2 的错误文案 —— 文案随版本变，而且本项目禁止
// 对中文/英文文案做子串匹配来判定语义。
func (s *Service) known(ctx context.Context, gid string) (bool, error) {
	live, err := s.liveStatuses(ctx)
	if err != nil {
		return false, err
	}
	for _, st := range live {
		if st.GID == gid {
			return true, nil
		}
	}
	if _, err := s.tasks.Get(ctx, gid); err == nil {
		return true, nil
	} else if !errors.Is(err, ErrNoTask) {
		return false, err
	}
	return false, nil
}

// Pause 暂停。控制动作以 aria2 为准：ARIA2 是执行方，面板的状态只是投影。
func (s *Service) Pause(ctx context.Context, gid string) error {
	return s.control(ctx, gid, func() error {
		if err := s.a2.Pause(ctx, gid); err != nil {
			return err
		}
		// 两边都要改：只动 aria2，面板重启后本地仍是旧状态；只动本地，aria2
		// 还在下载而界面显示已暂停。
		return s.tasks.SetState(ctx, gid, StatePaused)
	})
}

// Resume 继续。
func (s *Service) Resume(ctx context.Context, gid string) error {
	return s.control(ctx, gid, func() error {
		if err := s.a2.Resume(ctx, gid); err != nil {
			return err
		}
		return s.tasks.SetState(ctx, gid, StateActive)
	})
}

// Remove 删掉一条任务。force=true 走强删（aria2 会留下 .aria2 控制文件，
// 这是 aria2 的行为，需要用户明确选择，所以不默认发生）。
//
// aria2 对"已完成、它已经忘掉"的任务会报错，而这条记录在本地是真实存在的：
// 那种错误必须忽略，否则用户删不掉自己已完成的历史条目。
func (s *Service) Remove(ctx context.Context, gid string, force bool) error {
	ok, err := s.known(ctx, gid)
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("%w: %s", ErrNoTask, gid)
	}
	var rmErr error
	if force {
		rmErr = s.a2.ForceRemove(ctx, gid)
	} else {
		rmErr = s.a2.Remove(ctx, gid)
	}
	if rmErr != nil && !IsUnavailable(rmErr) {
		// aria2 说"没这个东西"是**预期内**的（已完成的任务它不留），不当失败。
		// 其它错误（超时、权限）则照实上抛：静默忽略会让用户以为删掉了，
		// 而任务还在跑。
		if !isAbsentError(rmErr) {
			return rmErr
		}
	}
	return s.tasks.SetTerminal(ctx, gid, StateRemoved, "", 0, 0)
}

// isAbsentError 认 aria2 的"这个 gid 我不认识"。
//
// 只在 Remove 里用，且判的是 aria2 的固定短语；判错的方向是安全的：把它当成
// 别的错误会让删除失败（用户重试即可），漏判也只是多报一个错误。
func isAbsentError(err error) bool {
	var ae *Error
	if !errors.As(err, &ae) {
		return false
	}
	m := strings.ToLower(ae.Message)
	return strings.Contains(m, "not present") || strings.Contains(m, "not found")
}

func (s *Service) control(ctx context.Context, gid string, fn func() error) error {
	ok, err := s.known(ctx, gid)
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("%w: %s", ErrNoTask, gid)
	}
	if err := fn(); err != nil {
		return err
	}
	s.wake()
	return nil
}

// ClearHistory 清掉已终结的历史记录。
func (s *Service) ClearHistory(ctx context.Context) (int, error) {
	return s.tasks.ClearHistory(ctx)
}

// Reconcile 在启动时把"面板以为还在进行、aria2 里已经查不到"的记录收口。
//
// 关键点：**aria2 不可达时一条都不收**。最容易写错的版本是"tellActive 报错就
// 当成没有任务"，那会让一次 aria2 重启把所有进行中的记录全标成失败，而它们
// 其实在 aria2 恢复后照常继续下。
func (s *Service) Reconcile(ctx context.Context) (int, error) {
	live, err := s.liveStatuses(ctx)
	if err != nil {
		return 0, fmt.Errorf("启动对账跳过（问不到 aria2）: %w", err)
	}
	alive := make(map[string]bool, len(live))
	for _, st := range live {
		alive[st.GID] = true
	}
	return s.tasks.Reconcile(ctx, func(gid string) bool { return alive[gid] })
}
