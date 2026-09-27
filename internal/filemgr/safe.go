// Package filemgr 文件管理（设计第 8 节）。
//
// D14 是这个包的前提：面板以 root 全盘访问，**不做根目录限制**。
// 因此这里没有任何"越界"检查 —— safe.go 只负责把一个字符串变成一个
// 确定的绝对路径，合法性只到"它是不是一个路径"为止。误操作的护栏
// 在别处：删除默认进回收站（8.6）。
package filemgr

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// ErrBadPath 表示输入根本不能被当作路径使用。API 层映射成 400。
//
// 刻意与"路径不存在"（fs.ErrNotExist，映射 404）分开：前者是用户在地址栏
// 写错了东西，后者是列表刷新与点击之间目录变了。两者的用户动作不同。
var ErrBadPath = errors.New("路径不合法")

// AbsClean 把用户给的路径变成确定的绝对路径：
//
//  1. 必须是绝对路径。相对路径一律拒绝 —— 按进程 cwd 解释会让同一个请求
//     参数在开发机（cwd=仓库）和目标机（cwd=/）指向不同目录，前端显示的
//     列表和实际操作的对象可能对不上。默认起始目录是上层的显式决定。
//  2. filepath.Clean：折叠 //、.、..、尾斜杠。
//  3. 解析符号链接，但**只解析存在的最长前缀**：
//     AbsClean 会被 mkdir/rename 用在"末段还不存在"的路径上，直接
//     EvalSymlinks 整条路径会在那里报 ENOENT，于是"新建文件"永远做不了。
//
// 解析符号链接不是为了圈禁，而是为了让路径落在用户眼睛看到的位置上。
//
// ⚠️ 具体语义按操作分两类，别搞混：
//   - 把路径当**目录浏览**（List）或当**位置**用（Mkdir 的父链）时，
//     需要的是解析后的真实位置 —— 用 AbsClean。
//   - 把路径当**被操作的对象**（Stat/Rename，以及将来的 Delete）时，
//     必须操作链接本身，末段不能解析 —— 用 SplitResolved。
//     AbsClean 在这类路径上会把链接解析成目标，"重命名一个链接"就变成
//     "搬走链接指向的文件"。
func AbsClean(p string) (string, error) {
	clean, err := cleanOnly(p)
	if err != nil {
		return "", err
	}
	return resolveExistingPrefix(clean)
}

// cleanOnly 只做校验 + Clean，**不解析任何符号链接**。
//
// 它是 SplitResolved 的地基：写入类操作要"末段原样、只解析中间层"，
// 所以必须有一个能把"清洗"和"解析"分开的入口。少了这一层，
// SplitResolved 只能先 AbsClean 整条路径 —— 那会把末段的符号链接也
// 解析成目标，于是"重命名一个链接"实际把链接指向的文件搬走了
// （ops_test.go 的 TestRenameSymlinkItself 就是这么抓到的）。
func cleanOnly(p string) (string, error) {
	if p == "" || strings.ContainsRune(p, 0) {
		return "", fmt.Errorf("%w: %q", ErrBadPath, p)
	}
	if !filepath.IsAbs(p) {
		return "", fmt.Errorf("%w: 必须是绝对路径 %q", ErrBadPath, p)
	}
	return filepath.Clean(p), nil
}

// resolveExistingPrefix 从整条路径开始逐级往上退，第一个 EvalSymlinks
// 成功的前缀就是"存在的最深前缀"，解析它再把剩余部分原样接回去。
//
// 为什么从最长往短试而不是从根往下拼：路径段数就是这里的循环次数，
// 而每一轮只有在"前缀真的存在"时才会起一次 EvalSymlinks；从根往下拼
// 要么每段都起一次系统调用（10 万条目目录里毫无必要），要么还是同样的
// 回溯逻辑但更难读。
//
// 唯一的边界：连 "/" 都解析不了（现实中不会发生）时原样返回清洗后的路径，
// 让后面的 os.* 去报真正的底层错误，这里不编造原因。
func resolveExistingPrefix(p string) (string, error) {
	rest := ""
	cur := p
	for {
		got, err := filepath.EvalSymlinks(cur)
		if err == nil {
			return filepath.Join(got, rest), nil
		}
		// 只有"路径不存在"才值得再往上退一级。
		// 权限不足（EACCES：中间某个 0700 的目录）不是"不存在"，继续退
		// 会把用户能读的文件解析到他没权限的父目录上去；符号链接环
		// （ELOOP）是用户自己能造出来的状态，得当成输入错误报出去。
		if !errors.Is(err, os.ErrNotExist) {
			return "", badPathErr(p, err)
		}
		parent := filepath.Dir(cur)
		if parent == cur { // 到根了
			return p, nil
		}
		rest = filepath.Join(filepath.Base(cur), rest)
		cur = parent
	}
}

func badPathErr(p string, cause error) error {
	return fmt.Errorf("%w: %q (%v)", ErrBadPath, p, cause)
}
