package service

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

// systemd 类型服务：面板只做 systemctl 的遥控器。
//
// 与 command 类型的根本区别（必须在行为上成立，不只是文档里写着）：
//   - 没有子进程可托管，因此不进 procs 表；
//   - 面板 Shutdown / Reconcile / Probe 一律不碰它 —— 用户选 SYSV 类型
//     就是为了"重启面板别牵连我的服务"；
//   - 状态是问出来的（is-active），不是等 wait4 等出来的。

// errNoSystemd 是给用户的说法，不是 Go 的原始错误。
// 容器、Termux 这些没有 systemd 的环境里，"systemctl: executable file
// not found in $PATH" 对一个想重启 nginx 的人来说没有任何信息量。
var errNoSystemd = errors.New("本机没有 systemd（systemctl 不可用），无法托管 systemd 类型服务")

// systemctlPath 每次现查而不是启动时缓存：测试靠改 PATH 注入假脚本，
// 缓存会让注入失效并被静默跳过。
func systemctlPath() (string, error) {
	p, err := exec.LookPath("systemctl")
	if err != nil {
		return "", errNoSystemd
	}
	return p, nil
}

// systemctl 执行一条命令，返回 stdout。
// 失败时把 systemctl 自己的输出拼进错误：它写的 "Unit nginx.service not
// found." 才是用户能看懂的原因，Go 的 exit status 不是。
func systemctl(ctx context.Context, args ...string) (string, error) {
	bin, err := systemctlPath()
	if err != nil {
		return "", err
	}
	if ctx == nil {
		ctx = context.Background()
	}
	cmd := exec.CommandContext(ctx, bin, args...)
	// 不经 shell：单元名是用户输入，走 shell 就等于把 "nginx; rm -rf /"
	// 交出去执行。
	out, err := cmd.Output()
	if err != nil {
		msg := strings.TrimSpace(string(out))
		var ee *exec.ExitError
		if errors.As(err, &ee) && len(ee.Stderr) > 0 {
			msg = strings.TrimSpace(msg + " " + string(ee.Stderr))
		}
		if msg == "" {
			msg = err.Error()
		}
		return "", fmt.Errorf("systemctl %s: %s", strings.Join(args, " "), msg)
	}
	return strings.TrimSpace(string(out)), nil
}

// systemctlLenient 与 systemctl 的差别只在退出码：非零但拿到了输出时照用。
// 专门给 is-active 这类"用退出码当返回值"的命令用；start/stop 不能用它，
// 否则真正的失败会被吞掉。
func systemctlLenient(ctx context.Context, args ...string) (string, error) {
	bin, err := systemctlPath()
	if err != nil {
		return "", err
	}
	out, runErr := exec.CommandContext(ctx, bin, args...).Output()
	outStr := strings.TrimSpace(string(out))
	if runErr != nil && outStr == "" {
		return "", fmt.Errorf("systemctl %s: %w", strings.Join(args, " "), runErr)
	}
	return outStr, nil
}

// ProbeUnit 查询单元当前状态。
func (s *Supervisor) ProbeUnit(svc Service) (State, error) {
	// is-active 用退出码表达状态：非 active 时退出码是 3，这是正常回答
	// 而不是失败。按通用错误处理的话，"没在跑"会变成"查询出错"。
	alive, err := systemctlLenient(context.Background(), "is-active", svc.Unit)
	if err != nil {
		return State{}, err
	}
	st := State{State: mapActive(alive)}
	if st.State == StateRunning {
		// MainPID 只在跑着的时候有意义。磁贴上那行 "PID xxxx" 是用户确认
		// "这是不是我以为的那个进程"的唯一依据，写 0 等于没写。
		if v, err := systemctl(context.Background(), "show", "-p", "MainPID", "--value", svc.Unit); err == nil {
			st.PID, _ = strconv.Atoi(v)
		}
		st.StartedAt = time.Now().Unix()
	}
	return st, nil
}

func mapActive(v string) StateName {
	switch v {
	case "active":
		return StateRunning
	case "activating":
		return StateRunning
	case "deactivating":
		return StateStopping
	default: // inactive / failed / unknown / 空
		return StateStopped
	}
}

// startSystemd 启动单元并把查到的真实状态落库。
func (s *Supervisor) startSystemd(svc Service) (State, error) {
	if _, err := systemctl(context.Background(), "start", svc.Unit); err != nil {
		return State{}, err
	}
	st, err := s.ProbeUnit(svc)
	if err != nil {
		return State{}, err
	}
	if err := SaveState(s.db, svc.ID, st); err != nil {
		return State{}, err
	}
	s.emit(Event{ID: svc.ID, State: st.State, PID: st.PID})
	return st, nil
}

// stopSystemd 停单元。grace 在这里没用上：优雅与否由 systemd 按单元的
// TimeoutStopSec 自己决定，面板再插一层 SIGTERM/SIGKILL 只会打乱它。
func (s *Supervisor) stopSystemd(svc Service) (State, error) {
	if _, err := systemctl(context.Background(), "stop", svc.Unit); err != nil {
		return State{}, err
	}
	st, err := s.ProbeUnit(svc)
	if err != nil {
		return State{}, err
	}
	st.Exit = &ExitInfo{Reason: ReasonClean, At: time.Now().Unix(), StoppedBy: StoppedByUser}
	if err := SaveState(s.db, svc.ID, st); err != nil {
		return State{}, err
	}
	s.emit(Event{ID: svc.ID, State: st.State, PID: st.PID})
	return st, nil
}
