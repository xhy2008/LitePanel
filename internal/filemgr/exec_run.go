package filemgr

// 任务分派：把一条 fs_jobs 记录映射到具体的执行体（设计 8.4 / M6-T4 第三阶段）。
//
// 这里是整套"关掉浏览器也照跑"的**唯一收口点**：前面 copy/move/delete 三套
// 内核、队列、持久层都是零件，只有这个函数被真正调用起来，链路才算通。也正
// 因为它是接线，出错的形态非常具体——某一支柱漏了、参数顺序倒了、permanent
// 传成 false——每一种都能让单元测试全绿而用户等到天荒地老，所以配套测试
// （exec_run_test.go）全部从"提交任务 → 观察盘上结果"外部验证。

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// runJob 按 op 分派。report 的语义与 progressFunc 一致：**累计**字节数与
// **累计**条目数（节流器拿它与上次做差，传增量会把进度算成天文数字）。
func (s *Service) runJob(ctx context.Context, j Job, report progressFunc) error {
	switch j.Op {
	case OpCopy:
		return s.runCopy(ctx, j, report)
	case OpMove:
		return s.runMove(ctx, j, report)
	case OpDelete:
		return s.runDelete(ctx, j, report)
	default:
		// 到不了这里：CreateJob 已经用 Op.valid() 挡过一遍。留着是因为
		// 库可以被人手工改（排障时 UPDATE 一条 op 是最常见的动作），而
		// "未支持的操作类型"比静默什么都不做诚实得多 —— 静默会让任务显示
		// done，用户以为文件已经搬走了。
		return fmt.Errorf("%w: 未支持的任务类型 %q", ErrJobInput, j.Op)
	}
}

// runCopy 把一个或多个源复制进 j.Dst（目录）。
func (s *Service) runCopy(ctx context.Context, j Job, report progressFunc) error {
	_, err := s.copyInto(ctx, j.Src, j.Dst, report)
	return err
}

// runMove 把一个或多个源移动到 j.Dst（目录）。
//
// 直接复用 MoveMany：它已经实现了"先全量校验再动手"的纪律（部分成功是移动
// 最坏的结果——没有回收站兜着，用户既不知道搬走了哪几个也退不回去）。
func (s *Service) runMove(ctx context.Context, j Job, report progressFunc) error {
	_, err := s.moveMany(ctx, j.Src, j.Dst, report, j.Resumed)
	return err
}

// runDelete 删除一批源，j.Permanent 决定是否进回收站。
//
// 进度传的是**累计**条目数（见 runJob 头注）：deleteMany 的 report 本就按
// 逐条累加调用，这里原样转发，删第 3 个时报 (0, 3)。写反成增量会让抽屉里
// 显示"已删 5000/12"。
func (s *Service) runDelete(ctx context.Context, j Job, report progressFunc) error {
	_, err := s.deleteMany(ctx, j.Src, j.Permanent, report)
	return err
}

// CopyInto 把若干源复制到目录 dstDir 之下，是 MoveTo/MoveMany 的对称孪生。
//
// 为什么要有个单独的对外入口而不是让调用方自己拼目标路径：复制的落点规则
// （dstDir/<basename(src)>、目录走 copyTree、文件走 copyFile、按类型分派）
// 必须和移动**完全一致**，否则同一个"粘贴"动作在复制时得到一个结果、移动时
// 得到另一个（最常见的是复制目录时忘了拒绝"目标已存在"，于是静默合并）。
func (s *Service) CopyInto(ctx context.Context, srcs []string, dstDir string) error {
	_, err := s.copyInto(ctx, srcs, dstDir, nopProgress)
	return err
}

// copyInto 是 CopyInto 的可上报进度内核，语义与 MoveMany 一一对应：
// 先全量校验（目标目录存在、每一个落点都不存在），再逐个执行。
//
// 先校验后执行对复制没有移动那么致命（失败时目标可以整份清掉），但它省掉
// 了"复制了 4 分钟到第 200 个文件才发现它已存在"这种体验——而这是用户在
// 一个装满备份的目录上粘贴时的**默认**情形。
func (s *Service) copyInto(ctx context.Context, srcs []string, dstDir string, report progressFunc) (int64, error) {
	if len(srcs) == 0 {
		return 0, fmt.Errorf("%w: 没有要复制的路径", ErrBadPath)
	}
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	dstDirReal, err := s.resolveDstDir(dstDir)
	if err != nil {
		return 0, err
	}
	type plan struct {
		src string
		dst string
		dir bool
	}
	plans := make([]plan, 0, len(srcs))
	seen := make(map[string]bool, len(srcs))
	for _, p := range srcs {
		srcReal, err := s.resolveSrcPath(p)
		if err != nil {
			return 0, err
		}
		dst := filepath.Join(dstDirReal, filepath.Base(srcReal))
		if dst == srcReal {
			return 0, fmt.Errorf("%w: 源与目标相同 (%s)", ErrBadPath, srcReal)
		}
		// 复制进自己的子树同样要拒（copyTree 内部也有这条，但那只能覆盖
		// 单棵子树；一批源里"目录 A"和"A/里/b"同时选中时靠它挡不住）。
		if srcIsDir(srcReal) && (dstDirReal == srcReal ||
			strings.HasPrefix(dstDirReal, srcReal+string(os.PathSeparator))) {
			return 0, fmt.Errorf("%w: 不能把 %s 复制到它自己之内", ErrBadPath, srcReal)
		}
		if seen[dst] {
			return 0, fmt.Errorf("%w: 这一批里有两个条目都叫 %s", ErrExists, filepath.Base(dst))
		}
		seen[dst] = true
		if _, err := os.Lstat(dst); err == nil {
			return 0, fmt.Errorf("%w: %s", ErrExists, dst)
		} else if !errors.Is(err, fs.ErrNotExist) {
			return 0, fmt.Errorf("检查目标 %s 失败: %w", dst, err)
		}
		plans = append(plans, plan{src: srcReal, dst: dst, dir: srcIsDir(srcReal)})
	}

	var done int64
	n := 0
	for _, pl := range plans {
		if err := ctx.Err(); err != nil {
			return done, err
		}
		var (
			got int64
			e2  error
		)
		if pl.dir {
			got, e2 = s.copyTree(ctx, pl.src, pl.dst, report)
		} else {
			got, e2 = s.copyFile(ctx, pl.src, pl.dst, report)
		}
		if e2 != nil {
			// 复制不整批回滚：与 MoveMany 同一条理由——回滚本身是另一轮
			// 可能失败的操作，而"删掉已经拷好的部分"这个动作在用户可能已经
			// 开始使用目标目录之后，破坏性不比留着更大。报"停在第几个"。
			return done, e2
		}
		done += got
		n++
		if err := report(done, n); err != nil {
			return done, err
		}
	}
	return done, nil
}
