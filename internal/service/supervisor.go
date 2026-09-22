package service

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"sync"
	"syscall"
	"time"

	"litepanel/internal/store"
)

// StopSource 表示这次停止是谁发起的，judgeExit 用它做归因。
type StopSource int

const (
	stopSelf     StopSource = iota // 进程自己结束的
	stopUser                       // 用户在面板上点了停止
	stopShutdown                   // 面板退出时的关停
)

// ErrNotRunning / ErrAlreadyRunning：调用方要区分"没在跑"和"跑着但停不掉"。
var (
	ErrNotRunning     = errors.New("服务未在运行")
	ErrAlreadyRunning = errors.New("服务已在运行")
)

// DefaultGrace 是 SIGTERM 到 SIGKILL 之间的宽限期。
const DefaultGrace = 10 * time.Second

// proc 是一个正在被监管的子进程。
type proc struct {
	cmd  *exec.Cmd
	pgid int
	buf  *LogBuf
	src  StopSource // 谁发起的停止；watcher 落库时读
	copy sync.WaitGroup
	done chan struct{}
}

// Supervisor 负责 command 类型服务的启停与状态落库。
type Supervisor struct {
	db    *store.DB
	mu    sync.Mutex
	procs map[int64]*proc
	// last 保留每个服务最近一次运行的日志缓冲。D19 只说日志不落盘，
	// 没说进程一退出就要忘掉 —— 崩溃后点开日志恰恰是最重要的用途。
	last  map[int64]*LogBuf
	limit int
}

// NewSupervisor 建监管器。
func NewSupervisor(db *store.DB) *Supervisor {
	return &Supervisor{db: db, procs: map[int64]*proc{}, last: map[int64]*LogBuf{}, limit: DefaultLogLines}
}

// SetLogLimit 改后续新建服务的日志行数上限。
func (s *Supervisor) SetLogLimit(n int) {
	if n > 0 {
		s.mu.Lock()
		s.limit = n
		for _, p := range s.procs {
			p.buf.Resize(n)
		}
		for _, b := range s.last {
			b.Resize(n)
		}
		s.mu.Unlock()
	}
}

// Log 返回服务的日志缓冲。没跑过的服务也返回空缓冲，API 层不必分叉。
func (s *Supervisor) Log(id int64) *LogBuf {
	s.mu.Lock()
	defer s.mu.Unlock()
	if p, ok := s.procs[id]; ok {
		return p.buf
	}
	if b, ok := s.last[id]; ok {
		return b
	}
	return NewLogBuf(s.limit)
}

// Start 拉起服务并落库 running。
//
// Setsid 让子进程自成会话与进程组：既能整组关停，也让 Pdeathsig 在
// 面板被 kill -9 时由内核收走它（D11）。
func (s *Supervisor) Start(svc Service) (State, error) {
	if svc.Kind != KindCommand {
		return State{}, fmt.Errorf("%s 类型不由面板托管启动", svc.Kind)
	}

	buf := NewLogBuf(s.limit)
	cmd := exec.Command("sh", "-c", svc.StartCmd)
	cmd.Dir = svc.Cwd
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true, Pdeathsig: syscall.SIGKILL}

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return State{}, err
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return State{}, err
	}

	p := &proc{cmd: cmd, buf: buf, done: make(chan struct{})}

	s.mu.Lock()
	if _, ok := s.procs[svc.ID]; ok {
		s.mu.Unlock()
		return State{}, ErrAlreadyRunning
	}
	s.procs[svc.ID] = p
	delete(s.last, svc.ID) // 新一次运行，旧日志作废
	s.mu.Unlock()

	if err := cmd.Start(); err != nil {
		s.forget(svc.ID)
		return State{}, fmt.Errorf("启动 %q: %w", svc.Name, err)
	}
	// Setsid 之后子进程自己就是组长：pgid == pid。
	p.pgid = cmd.Process.Pid

	p.copy.Add(2)
	go func() { defer p.copy.Done(); _, _ = io.Copy(buf, stdout) }()
	go func() { defer p.copy.Done(); _, _ = io.Copy(buf, stderr) }()
	go s.watch(svc.ID, p)

	st := State{
		State:     StateRunning,
		PID:       cmd.Process.Pid,
		PGID:      p.pgid,
		StartedAt: time.Now().Unix(),
	}
	if err := SaveState(s.db, svc.ID, st); err != nil {
		return State{}, err
	}
	return st, nil
}

// watch 等子进程退出，把退出码与归因写回 DB。
func (s *Supervisor) watch(id int64, p *proc) {
	defer close(p.done)

	// 非零退出不是 error，是数据；真正的启动失败在 Start 里已经报过了。
	_ = p.cmd.Wait()
	// Wait 会在管道拷贝完成前返回；不等一下就会丢掉最后几行日志。
	p.copy.Wait()

	var ws syscall.WaitStatus
	if p.cmd.ProcessState != nil {
		if got, ok := p.cmd.ProcessState.Sys().(syscall.WaitStatus); ok {
			ws = got
		}
	}

	s.mu.Lock()
	src := p.src
	delete(s.procs, id)
	s.last[id] = p.buf
	s.mu.Unlock()

	exit := judgeExit(ws, src)
	exit.At = time.Now().Unix()
	_ = SaveState(s.db, id, State{State: StateStopped, Exit: &exit})
}

// Stop 停服务：自定义停止命令 → SIGTERM 整组 → 宽限期 → SIGKILL 整组。
func (s *Supervisor) Stop(ctx context.Context, svc Service, grace time.Duration) (State, error) {
	s.mu.Lock()
	p, ok := s.procs[svc.ID]
	if ok {
		p.src = stopUser
	}
	s.mu.Unlock()
	if !ok {
		return State{}, ErrNotRunning
	}
	return s.terminate(ctx, svc, p, grace)
}

// terminate 是 Stop 与 Shutdown 共用的关停流程。
func (s *Supervisor) terminate(ctx context.Context, svc Service, p *proc, grace time.Duration) (State, error) {
	if grace <= 0 {
		grace = DefaultGrace
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if d, ok := ctx.Deadline(); ok {
		if left := time.Until(d) / 2; left < grace && left > 0 {
			grace = left // 别超过调用方给的总时限
		}
	}

	_ = SaveState(s.db, svc.ID, State{
		State: StateStopping, PID: p.cmd.Process.Pid, PGID: p.pgid,
	})

	// 自定义停止命令优先：不少服务只认自己的关停脚本（先摘流量再退出）。
	if svc.StopCmd != "" {
		c := exec.CommandContext(ctx, "sh", "-c", svc.StopCmd)
		c.Dir = svc.Cwd
		_ = c.Run()
	}

	pgid := p.pgid
	// 整组发信号：setsid 保证这一组里没有别人。
	_ = syscall.Kill(-pgid, syscall.SIGTERM)

	select {
	case <-p.done:
	case <-time.After(grace):
		_ = syscall.Kill(-pgid, syscall.SIGKILL)
		select {
		case <-p.done:
		case <-time.After(5 * time.Second):
			return State{}, fmt.Errorf("服务 %s 在 SIGKILL 后仍未回收", svc.Name)
		}
	}
	return GetState(s.db, svc.ID)
}

// Shutdown 停掉所有在跑的服务（面板正常退出路径）。
func (s *Supervisor) Shutdown(ctx context.Context) error {
	s.mu.Lock()
	type entry struct {
		id int64
		p  *proc
	}
	var list []entry
	for id, p := range s.procs {
		p.src = stopShutdown
		list = append(list, entry{id, p})
	}
	s.mu.Unlock()

	for _, e := range list {
		svc, err := Get(s.db, e.id)
		if err != nil {
			continue // 服务已被删掉，进程照样会被 terminate 收走
		}
		if _, err := s.terminate(ctx, svc, e.p, DefaultGrace); err != nil {
			return err
		}
	}
	return nil
}

// Reconcile 启动对账：清掉上次面板留下的脏状态，并收走仍活着的孤儿进程。
//
// D11：面板被 kill -9 时内核已经用 Pdeathsig 收走了进程组；这里还看到
// running，说明是极窄竞态留下的。这些进程归我们所有，可以直接整组杀。
func (s *Supervisor) Reconcile(ctx context.Context) error {
	list, err := List(s.db)
	if err != nil {
		return err
	}
	for _, svc := range list {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if svc.Kind != KindCommand {
			continue
		}
		st, err := GetState(s.db, svc.ID)
		if err != nil {
			return err
		}
		if st.State != StateRunning && st.State != StateStarting && st.State != StateStopping {
			continue
		}
		if st.PID > 0 {
			pg := st.PGID
			if pg == 0 {
				pg = st.PID
			}
			_ = syscall.Kill(-pg, syscall.SIGKILL)
		}
		exit := ExitInfo{
			Code: 137, Signal: int(syscall.SIGKILL), Reason: ReasonError,
			At: time.Now().Unix(), StoppedBy: StoppedByPdeathsig,
		}
		if err := SaveState(s.db, svc.ID, State{State: StateStopped, Exit: &exit}); err != nil {
			return err
		}
	}
	return nil
}

// Probe 校验 DB 里声称在跑、但内存里没有对应进程的行。
//
// 这种情况意味着 pid 已经不是我们记录的那个进程（PID 被回收复用）。
// 只清状态，绝不 kill —— 那个 pid 现在属于别人的进程，误杀是灾难。
func (s *Supervisor) Probe() (int, error) {
	list, err := List(s.db)
	if err != nil {
		return 0, err
	}
	fixed := 0
	for _, svc := range list {
		if svc.Kind != KindCommand {
			continue
		}
		st, err := GetState(s.db, svc.ID)
		if err != nil {
			return 0, err
		}
		if st.State != StateRunning && st.State != StateStarting {
			continue
		}
		s.mu.Lock()
		p, owned := s.procs[svc.ID]
		s.mu.Unlock()
		if owned && p.cmd.Process != nil && p.cmd.Process.Pid == st.PID {
			continue // 我们自己在跑的进程
		}
		exit := ExitInfo{
			Reason: ReasonError, At: time.Now().Unix(), StoppedBy: StoppedBySelf,
		}
		if st.Exit != nil {
			exit.Code = st.Exit.Code
			exit.Signal = st.Exit.Signal
		}
		if err := SaveState(s.db, svc.ID, State{State: StateStopped, Exit: &exit}); err != nil {
			return 0, err
		}
		fixed++
	}
	return fixed, nil
}

func (s *Supervisor) forget(id int64) {
	s.mu.Lock()
	delete(s.procs, id)
	s.mu.Unlock()
}

// judgeExit 把 wait 状态翻译成 D21 要求的三种呈现之一：
//
//	自己退出 0            → clean，code 0
//	自己退出非 0          → error，code = 退出码
//	被信号打死            → error，code = 128+signo（OOM 137、段错误 139）
//	用户/面板关停后响应 SIGTERM → clean（这是"点关闭"的正常路径）
//	用户/面板关停后被 SIGKILL   → error（宽限期内没走，属于异常）
func judgeExit(ws syscall.WaitStatus, src StopSource) ExitInfo {
	by := StoppedBySelf
	switch src {
	case stopUser:
		by = StoppedByUser
	case stopShutdown:
		by = StoppedByPanelShutdown
	}

	if ws.Exited() {
		code := ws.ExitStatus()
		reason := ReasonClean
		if code != 0 {
			reason = ReasonError
		}
		return ExitInfo{Code: code, Reason: reason, StoppedBy: by}
	}

	sig := int(ws.Signal())
	// 没人叫它停却被 SIGKILL：基本只有 OOM killer 或父进程死掉的 PDEATHSIG。
	if src == stopSelf && ws.Signal() == syscall.SIGKILL {
		by = StoppedByPdeathsig
	}
	reason := ReasonError
	if (src == stopUser || src == stopShutdown) && ws.Signal() == syscall.SIGTERM {
		reason = ReasonClean
	}
	return ExitInfo{Code: 128 + sig, Signal: sig, Reason: reason, StoppedBy: by}
}
