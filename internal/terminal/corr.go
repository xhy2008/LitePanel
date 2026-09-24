package terminal

import (
	"errors"
	"strings"
	"sync"
)

// corr 把「一条待发命令」和它的响应块对上号。
//
// 为什么必须比命令编号（集成测试实际踩到的）：attach 之后 tmux 会先发一
// 个握手块（%begin <ts> N 0 / %end <ts> N 0）。如果它在我们已经把命令写
// 进连接之后才被读到，而 %end 不看编号就认，那么握手块的空 %end 会把等待
// 者提前叫醒并返回「成功」，真正的 %error 到达时已无人接收 —— 表现是
// 「命令明明失败，调用方拿到 nil」。
//
// 关联规则：写完命令后的**第一个** %begin 就是我们的（control mode 严格
// FIFO），记下它的编号，之后的数据与 %end/%error 只认这个编号。前提是
// Session 已经过了握手块（见 ready），否则"第一个 %begin"可能是握手的。
type corr struct {
	mu       sync.Mutex // accept 在 readLoop，abort 可能在 Run 的超时分支
	num      string     // "" = 还没认领到 %begin
	data     []byte
	err      error
	done     chan struct{}
	resolved bool
}

// accept 判断事件是否属于我们这个块；true 表示已消费，不应外抛。
func (c *corr) accept(e Event) bool {
	if c == nil {
		return false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.resolved {
		return false
	}
	switch e.Kind {
	case EvBegin:
		if c.num == "" {
			c.num = nthField(e.Arg, 1)
			return true
		}
		return false
	case EvBlockData:
		if c.num != "" {
			c.data = append(c.data, e.Data...)
			return true
		}
		return false
	case EvEnd, EvError:
		if c.num == "" || nthField(e.Arg, 1) != c.num {
			return false // 无主块，或编号不符 —— 让它外抛
		}
		if e.Kind == EvError {
			c.err = errors.New(strings.TrimSpace(string(c.data)))
		}
		c.resolve()
		return true
	}
	return false
}

// resolve 调用者必须持锁。
func (c *corr) resolve() {
	if c.resolved {
		return
	}
	c.resolved = true
	close(c.done)
}

// abort 在调用方放弃等待（超时/断线）时收尾：之后迟到的响应被当无主块。
func (c *corr) abort(err error) {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.resolved {
		return
	}
	c.err = err
	c.resolve()
}

// resolvedNow 给 dispatch 用：认领完成后要把 s.pending 清空。
func (c *corr) resolvedNow() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.resolved
}
