package terminal

import (
	"fmt"
	"os"
	"syscall"

	"golang.org/x/sys/unix"
)

// openPty 开一对 PTY，返回 master 与 slave。
//
// 不引第三方库：Linux 上就是 /dev/ptmx + 两个 ioctl（TIOCSPTLCK 解锁、
// TIOCGPTN 取号）再开 /dev/pts/N。unix.IoctlSetPointerInt / IoctlGetInt
// 已经把两个 ioctl 封好了，为一个"打开两个设备文件"引 creack/pty 不
// 划算（而且这机器上下不了新模块）。
// OpenPty 暴露给同仓库的开发探针（dev/）与内部集成测试用。
func OpenPty() (master, slave *os.File, err error) { return openPty() }

func openPty() (master, slave *os.File, err error) {
	master, err = os.OpenFile("/dev/ptmx", os.O_RDWR|syscall.O_NOCTTY, 0)
	if err != nil {
		return nil, nil, fmt.Errorf("打开 /dev/ptmx: %w", err)
	}
	if err := unix.IoctlSetPointerInt(int(master.Fd()), unix.TIOCSPTLCK, 0); err != nil {
		master.Close()
		return nil, nil, fmt.Errorf("TIOCSPTLCK: %w", err)
	}
	n, err := unix.IoctlGetInt(int(master.Fd()), unix.TIOCGPTN)
	if err != nil {
		master.Close()
		return nil, nil, fmt.Errorf("TIOCGPTN: %w", err)
	}
	slave, err = os.OpenFile(fmt.Sprintf("/dev/pts/%d", n), os.O_RDWR|syscall.O_NOCTTY, 0)
	if err != nil {
		master.Close()
		return nil, nil, fmt.Errorf("打开 /dev/pts/%d: %w", n, err)
	}
	return master, slave, nil
}
