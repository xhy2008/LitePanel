package service

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"litepanel/internal/store"
)

// alive 独立于被测代码判断进程是否还在跑。
// 注意：被杀但没被父进程 wait 的进程会变成僵尸，/proc/<pid> 依然存在，
// 所以必须读状态位，僵尸算已死。
func alive(pid int) bool {
	if pid <= 0 {
		return false
	}
	b, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return false
	}
	// comm 字段可能含空格与括号，状态位在最后一个 ')' 之后。
	i := strings.LastIndex(string(b), ")")
	if i < 0 || i+2 >= len(b) {
		return false
	}
	return b[i+2] != 'Z'
}

func sidOf(t *testing.T, pid int) int {
	t.Helper()
	b, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		t.Fatalf("读 /proc/%d/stat: %v", pid, err)
	}
	// comm 字段可能含空格与括号，必须从最后一个 ')' 之后再切。
	i := strings.LastIndex(string(b), ")")
	fields := strings.Fields(string(b)[i+1:])
	// fields[0]=state [1]=ppid [2]=pgrp [3]=session
	pgid, err := strconv.Atoi(fields[2])
	if err != nil {
		t.Fatalf("解析 pgid: %v", err)
	}
	sid, err := strconv.Atoi(fields[3])
	if err != nil {
		t.Fatalf("解析 sid: %v", err)
	}
	_ = pgid
	return sid
}

// orphanCmd 造一个"上次面板留下的"游离进程：自成会话、写回自己的 pid 后长睡。
func orphanCmd(body, pidFile string) *exec.Cmd {
	cmd := exec.Command("sh", "-c", fmt.Sprintf("echo $$ > %s; %s", pidFile, body))
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	return cmd
}

func newSup(t *testing.T) (*Supervisor, *store.DB) {
	t.Helper()
	db := openDB(t)
	s := NewSupervisor(db)
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = s.Shutdown(ctx)
	})
	return s, db
}

func addSvc(t *testing.T, db *store.DB, cmd string) Service {
	t.Helper()
	svc, err := Create(db, ServiceInput{
		Name: fmt.Sprintf("s%d", time.Now().UnixNano()), Kind: KindCommand, StartCmd: cmd,
	})
	if err != nil {
		t.Fatal(err)
	}
	return svc
}

// waitExit 等 watcher 把结果落库，返回 DB 里的状态。
func waitExit(t *testing.T, db *store.DB, id int64) State {
	t.Helper()
	deadline := time.Now().Add(8 * time.Second)
	for time.Now().Before(deadline) {
		st, err := GetState(db, id)
		if err != nil {
			t.Fatal(err)
		}
		if st.State == StateStopped {
			return st
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("等待服务落到 stopped 超时")
	return State{}
}

// ---- 子进程创建（R5）----

// R5：子进程必须自成会话，否则它跟着面板的进程组，面板被 kill -9
// 时会被一起拖掉或反过来变成孤儿继续跑，两种都不是想要的。
func TestStartSpawnsDetachedSession(t *testing.T) {
	s, db := newSup(t)
	svc := addSvc(t, db, "sleep 3000")

	st, err := s.Start(svc)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if st.PID <= 0 {
		t.Fatalf("未记录 PID: %+v", st)
	}
	if st.PGID != st.PID {
		t.Fatalf("PGID 应等于 PID（Setsid 生效）: %+v", st)
	}
	if sid := sidOf(t, st.PID); sid != st.PID {
		t.Fatalf("SID=%d 应等于 PID=%d", sid, st.PID)
	}
}

// 大量线程churn 曾经会误杀子进程（Go 回收孵化线程）。这条防的是那个回归。
func lockUnlockThread() {
	runtime.LockOSThread()
	runtime.UnlockOSThread()
	time.Sleep(time.Millisecond)
}

func TestSpawnSurvivesThreadChurn(t *testing.T) {
	s, db := newSup(t)
	svc := addSvc(t, db, "sleep 3000")
	st, err := s.Start(svc)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	for i := 0; i < 32; i++ {
		go func() {
			lockUnlockThread()
			done <- struct{}{}
		}()
	}
	for i := 0; i < 32; i++ {
		<-done
	}
	if !alive(st.PID) {
		t.Fatal("线程 churn 后子进程消失了")
	}
}

func TestChildCwdHonored(t *testing.T) {
	s, db := newSup(t)
	dir := t.TempDir()
	marker := filepath.Join(dir, "pid.txt")
	svc, err := Create(db, ServiceInput{
		Name: "cwd", Kind: KindCommand, StartCmd: "pwd > pid.txt; sleep 3000", Cwd: dir,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Start(svc); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if b, err := os.ReadFile(marker); err == nil && len(b) > 0 {
			if strings.TrimSpace(string(b)) != dir {
				t.Fatalf("cwd 不对: %q want %q", strings.TrimSpace(string(b)), dir)
			}
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("子进程没在指定 cwd 下写出文件")
}

// ---- 启停 ----

func TestStartWritesRunningState(t *testing.T) {
	s, db := newSup(t)
	svc := addSvc(t, db, "sleep 3000")
	st, err := s.Start(svc)
	if err != nil {
		t.Fatal(err)
	}
	got, err := GetState(db, svc.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.State != StateRunning || got.PID != st.PID || got.StartedAt == 0 {
		t.Fatalf("DB 状态没写对: %+v", got)
	}
}

func TestStartRejectsAlreadyRunning(t *testing.T) {
	s, db := newSup(t)
	svc := addSvc(t, db, "sleep 3000")
	if _, err := s.Start(svc); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Start(svc); err == nil {
		t.Fatal("重复启动应报错，而不是留下两个进程")
	}
}

func TestStopKillsWholeGroup(t *testing.T) {
	s, db := newSup(t)
	dir := t.TempDir()
	// 子 shell 里再开一个 sleep；停止必须连孙子一起收走。
	svc, err := Create(db, ServiceInput{
		Name:     "group",
		Kind:     KindCommand,
		StartCmd: "sleep 3000 & echo $! > kid.txt; wait",
		Cwd:      dir,
	})
	if err != nil {
		t.Fatal(err)
	}
	st, err := s.Start(svc)
	if err != nil {
		t.Fatal(err)
	}
	kid := waitForFileInt(t, filepath.Join(dir, "kid.txt"), 5*time.Second)
	if !alive(kid) {
		t.Fatalf("孙子进程本该在跑, pid=%d", kid)
	}
	if _, err := s.Stop(context.Background(), svc, 2*time.Second); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	// 整个进程组都必须消失。
	waitGone(t, st.PID, "组长进程")
	waitGone(t, kid, "孙子进程")
}

// D21：用户在面板上点停止 → 视为正常退出。
func TestStopByUserIsClean(t *testing.T) {
	s, db := newSup(t)
	svc := addSvc(t, db, "sleep 3000")
	if _, err := s.Start(svc); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Stop(context.Background(), svc, 2*time.Second); err != nil {
		t.Fatal(err)
	}
	st := waitExit(t, db, svc.ID)
	if st.Exit == nil {
		t.Fatal("缺退出信息")
	}
	if st.Exit.Reason != ReasonClean {
		t.Fatalf("用户停止应判 clean, got %+v", st.Exit)
	}
	if st.Exit.StoppedBy != StoppedByUser {
		t.Fatalf("归因应为 user, got %q", st.Exit.StoppedBy)
	}
}

// 宽限期内不退出要升级到 SIGKILL，且不能谎报成正常退出。
func TestStopEscalatesToSigkill(t *testing.T) {
	s, db := newSup(t)
	dir := t.TempDir()
	ready := filepath.Join(dir, "ready")
	svc, err := Create(db, ServiceInput{
		Name: "stubborn", Kind: KindCommand, Cwd: dir,
		// 先装 trap 再报就绪：否则信号可能赶在 trap 之前到达，
		// 那样测的是"响应 TERM"而不是"宽限期后升级 KILL"。
		StartCmd: "trap '' TERM; touch " + ready + "; while :; do sleep 0.2; done",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Start(svc); err != nil {
		t.Fatal(err)
	}
	waitForFile(t, ready, 5*time.Second)
	start := time.Now()
	if _, err := s.Stop(context.Background(), svc, 300*time.Millisecond); err != nil {
		t.Fatal(err)
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("未在宽限期后升级 SIGKILL, 耗时 %v", elapsed)
	}
	st := waitExit(t, db, svc.ID)
	if st.Exit == nil || st.Exit.Reason != ReasonError {
		t.Fatalf("被 SIGKILL 干掉不该是 clean: %+v", st.Exit)
	}
	if st.Exit.StoppedBy != StoppedByUser {
		t.Fatalf("归因应为 user, got %q", st.Exit.StoppedBy)
	}
}

func TestStopNotRunning(t *testing.T) {
	s, db := newSup(t)
	svc := addSvc(t, db, "sleep 3000")
	if _, err := s.Stop(context.Background(), svc, time.Second); err != ErrNotRunning {
		t.Fatalf("未运行时停止应得 ErrNotRunning, got %v", err)
	}
}

// 自定义 stop_cmd 存在时优先用它（很多服务只认自己的关停脚本）。
func TestCustomStopCmd(t *testing.T) {
	s, db := newSup(t)
	dir := t.TempDir()
	marker := filepath.Join(dir, "stopped")
	svc, err := Create(db, ServiceInput{
		Name:     "custom",
		Kind:     KindCommand,
		StartCmd: "sleep 3000",
		StopCmd:  "touch " + marker,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Start(svc); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Stop(context.Background(), svc, 2*time.Second); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("自定义停止命令没执行: %v", err)
	}
}

// ---- 输出捕获 ----

func TestStartCapturesOutput(t *testing.T) {
	s, db := newSup(t)
	svc := addSvc(t, db, "echo 你好; echo 错误 >&2; exit 0")
	if _, err := s.Start(svc); err != nil {
		t.Fatal(err)
	}
	waitExit(t, db, svc.ID)
	lines := s.Log(svc.ID).Tail(100)
	joined := strings.Join(lines, "|")
	if !strings.Contains(joined, "你好") || !strings.Contains(joined, "错误") {
		t.Fatalf("stdout 与 stderr 都要进缓冲, got %v", lines)
	}
}

// D19：缓冲只在内存。这条断言的是"没写数据库之外的地方" ——
// 服务的数据目录里除了 marker 不该多出任何文件。
func TestLogBufNotPersisted(t *testing.T) {
	s, db := newSup(t)
	dir := t.TempDir()
	svc, err := Create(db, ServiceInput{
		Name: "log", Kind: KindCommand, StartCmd: "echo a; echo b; sleep 3000", Cwd: dir,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Start(svc); err != nil {
		t.Fatal(err)
	}
	waitForLines(t, s.Log(svc.ID), 2, 5*time.Second)

	ents, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(ents) != 0 {
		var names []string
		for _, e := range ents {
			names = append(names, e.Name())
		}
		t.Fatalf("服务目录里出现了文件，日志可能被落盘了: %v", names)
	}
}

// ---- 退出码判定（D21/T6）----

func TestJudgeExit(t *testing.T) {
	// wait4 状态构造：退出码走低字节，信号致死走低位。
	made := func(exited bool, code, sig int) syscall.WaitStatus {
		if exited {
			return syscall.WaitStatus(code << 8)
		}
		return syscall.WaitStatus(sig)
	}
	cases := []struct {
		name    string
		ws      syscall.WaitStatus
		reason  Reason
		code    int
		sig     int
		by      string
		stopped StopSource
	}{
		{"退出 0 → 正常", made(true, 0, 0), ReasonClean, 0, 0, StoppedBySelf, stopSelf},
		{"退出 3 → 异常", made(true, 3, 0), ReasonError, 3, 0, StoppedBySelf, stopSelf},
		{"用户停止且退出 0 → 正常", made(true, 0, 0), ReasonClean, 0, 0, StoppedByUser, stopUser},
		{"用户停止但 SIGTERM 致死 → 正常", made(false, 0, int(syscall.SIGTERM)),
			ReasonClean, 143, int(syscall.SIGTERM), StoppedByUser, stopUser},
		{"段错误 → 异常 139", made(false, 0, int(syscall.SIGSEGV)),
			ReasonError, 139, int(syscall.SIGSEGV), StoppedBySelf, stopSelf},
		{"OOM 击杀(SIGKILL) → 异常 137", made(false, 0, int(syscall.SIGKILL)),
			ReasonError, 137, int(syscall.SIGKILL), StoppedByPdeathsig, stopSelf},
		{"宽限期后被面板 SIGKILL → 异常且归因 shutdown", made(false, 0, int(syscall.SIGKILL)),
			ReasonError, 137, int(syscall.SIGKILL), StoppedByPanelShutdown, stopShutdown},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := judgeExit(c.ws, c.stopped)
			if got.Reason != c.reason || got.Code != c.code || got.Signal != c.sig {
				t.Fatalf("判定错: %+v, want reason=%s code=%d sig=%d",
					got, c.reason, c.code, c.sig)
			}
			// D21：只有 clean / error 两种，不存在第三种。
			if got.Reason != ReasonClean && got.Reason != ReasonError {
				t.Fatalf("出现第三种 reason: %q", got.Reason)
			}
		})
	}
}

// 真实进程链路也要覆盖一遍：脚本自己 exit 3，UI 才能显示「异常退出 · code 3」。
func TestExitCodeFromRealProcess(t *testing.T) {
	s, db := newSup(t)
	svc := addSvc(t, db, "exit 3")
	if _, err := s.Start(svc); err != nil {
		t.Fatal(err)
	}
	st := waitExit(t, db, svc.ID)
	if st.Exit == nil || st.Exit.Reason != ReasonError || st.Exit.Code != 3 {
		t.Fatalf("应记录异常退出 code 3: %+v", st.Exit)
	}
}

func TestShutdownKillsEverything(t *testing.T) {
	s, db := newSup(t)
	var pids []int
	for i := 0; i < 3; i++ {
		svc := addSvc(t, db, "sleep 3000")
		st, err := s.Start(svc)
		if err != nil {
			t.Fatal(err)
		}
		pids = append(pids, st.PID)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := s.Shutdown(ctx); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
	for _, pid := range pids {
		if alive(pid) {
			t.Fatalf("Shutdown 后进程仍在: %d", pid)
		}
	}
}

// D11 收尾：上次面板留下的孤儿进程要在启动对账时被收走，状态清成 stopped。
func TestReconcileKillsOrphan(t *testing.T) {
	_, db := newSup(t)
	svc := addSvc(t, db, "sleep 3000")

	// 手工造一个"上次面板留下的"进程：自成会话，写 pid 后长睡。
	dir := t.TempDir()
	cmd := orphanCmd("sleep 3000", filepath.Join(dir, "pid"))
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	pid := waitForFileInt(t, filepath.Join(dir, "pid"), 5*time.Second)
	if err := SaveState(db, svc.ID, State{
		State: StateRunning, PID: pid, PGID: pid, StartedAt: time.Now().Unix(),
	}); err != nil {
		t.Fatal(err)
	}

	s := NewSupervisor(db)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := s.Reconcile(ctx); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	waitGone(t, pid, "孤儿进程")
	st, _ := GetState(db, svc.ID)
	if st.State != StateStopped || st.PID != 0 {
		t.Fatalf("状态未清成 stopped: %+v", st)
	}
}

// 状态探测：pid 还活着，但那个进程不是我们记录的（PID 被回收复用）。
// 绝不能 kill 它 —— 那是别人的进程；只能把 DB 行清成 stopped。
func TestProbeDetectsPidReuse(t *testing.T) {
	s, db := newSup(t)
	svc := addSvc(t, db, "sleep 3000")

	// 内存里没有这个服务的进程，但 DB 声称在跑，且 pid 指向一个确定活着、
	// 进程组也对不上的进程（面板自己）。
	if err := SaveState(db, svc.ID, State{
		State: StateRunning, PID: os.Getpid(), PGID: os.Getpid() + 7777, StartedAt: 1,
	}); err != nil {
		t.Fatal(err)
	}
	n, err := s.Probe()
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("应判定 1 条残留, got %d", n)
	}
	got, _ := GetState(db, svc.ID)
	if got.State != StateStopped || got.PID != 0 {
		t.Fatalf("应清成 stopped: %+v", got)
	}
	// 面板自己当然还活着：误杀就是灾难。
	if !alive(os.Getpid()) {
		t.Fatal("探测过程把无关进程杀了")
	}
}

// 我们自己在跑的服务不能被探测误伤。
func TestProbeKeepsLiveService(t *testing.T) {
	s, db := newSup(t)
	svc := addSvc(t, db, "sleep 3000")
	if _, err := s.Start(svc); err != nil {
		t.Fatal(err)
	}
	if n, err := s.Probe(); err != nil {
		t.Fatal(err)
	} else if n != 0 {
		t.Fatalf("活服务不该被动, got %d", n)
	}
	got, _ := GetState(db, svc.ID)
	if got.State != StateRunning {
		t.Fatalf("状态被探测改坏了: %+v", got)
	}
}

// ---- 辅助 ----

// waitGone 等进程真正消失。杀进程组时信号传播到每个成员需要一点时间。
func waitForFile(t *testing.T, path string, d time.Duration) {
	t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(path); err == nil {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("等 %s 超时", path)
}

func waitGone(t *testing.T, pid int, what string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for alive(pid) && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if alive(pid) {
		t.Fatalf("%s 未消失: pid=%d", what, pid)
	}
}

func waitForFileInt(t *testing.T, path string, d time.Duration) int {
	t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if b, err := os.ReadFile(path); err == nil {
			if n, err := strconv.Atoi(strings.TrimSpace(string(b))); err == nil && n > 0 {
				return n
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("等 %s 超时", path)
	return 0
}

func waitForLines(t *testing.T, b *LogBuf, n int, d time.Duration) {
	t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if b.Len() >= n {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("等待 %d 行日志超时, 当前 %d", n, b.Len())
}

// WS 帧的 key 必须是 snake_case：前端坚持 HTTP 与 WS 共用一套字段名，
// 缺 tag 就会吐 PascalCase，而前端读不到时只是不更新，不报错 ——
// 表现为"服务崩了 UI 仍显示运行中"。
func TestEventJSONIsSnakeCase(t *testing.T) {
	sig := 9
	code := 137
	raw, err := json.Marshal(Event{
		ID: 7, State: StateStopped,
		Exit: &ExitInfo{Code: code, Signal: sig, Reason: ReasonError,
			At: 1700000000, StoppedBy: StoppedByPdeathsig},
	})
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"id", "state", "pid", "exit"} {
		if _, ok := m[k]; !ok {
			t.Fatalf("缺字段 %q: %s", k, raw)
		}
	}
	exit, ok := m["exit"].(map[string]any)
	if !ok {
		t.Fatalf("exit 没序列化出来: %s", raw)
	}
	for _, k := range []string{"code", "signal", "reason", "at", "stopped_by"} {
		if _, ok := exit[k]; !ok {
			t.Fatalf("exit 缺字段 %q: %s", k, raw)
		}
	}
}
