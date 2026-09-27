package filemgr

import (
	"context"

	"litepanel/internal/metrics"
)

// Service 是文件管理的全部能力对外的入口（api.Files 的实现）。
type Service struct {
	// usage 是 statfs 的注入点（默认 metrics.DiskUsage）。
	//
	// 之所以非要有注入点：夹具挂载表里的 /DISK、/home 在跑测试的机器上
	// 通常不存在，真 statfs 必然失败。如果实现因此丢掉这些条目，"挂载
	// 枚举"的测试就只能断言"本机恰好有 /" —— 那测的是运行环境，不是代码。
	usage usageFunc
	// procDir 是 /proc 的位置，只有 Roots 用它（读 mounts 表）。
	// 可注入是为了测试能喂一份固定的挂载表：真实 /proc/mounts 的内容
	// 取决于跑测试的机器（本机是 Android/Termux，171 行、根是 erofs 只读），
	// 直接读它的话"roots 必须包含 /"这类断言会在某些机器上因为环境
	// 恰好不合而变红，而那不是被测代码的错。
	procDir string
}

// Options 是装配参数。零值可用（procDir 默认 "/proc"）。
type Options struct {
	ProcDir string
	// DiskUsage 覆盖 statfs 实现（测试注入）。nil = 真实调用。
	DiskUsage func(string) (diskUsage, error)
}

// NewService 装配文件管理服务。
func NewService(opts Options) *Service {
	dir := opts.ProcDir
	if dir == "" {
		dir = "/proc"
	}
	usage := opts.DiskUsage
	if usage == nil {
		usage = realUsage
	}
	return &Service{procDir: dir, usage: usage}
}

// diskUsage 是容量快照的本地别名 —— 不直接把 metrics.Usage 暴露成
// JSON 契约类型：那会让 api 层的响应格式取决于 metrics 包的字段名，
// 而那边是给磁盘进度条用的，两边演进方向不同。
type diskUsage = metrics.Usage

type usageFunc func(string) (diskUsage, error)

func realUsage(p string) (diskUsage, error) { return metrics.DiskUsage(p) }

// ProcDir 暴露注入值，供装配层与测试确认自己接的是哪一个 /proc。
func (s *Service) ProcDir() string { return s.procDir }

// List 是包级 List 的方法形态，好让 Service 满足 api.Files 接口。
//
// 为什么不把实现搬进方法：browse_test.go 里几十个断言都直接调
// List(ctx, dir, opts)，方法化会让单元测试必须先造一个 Service ——
// 而列举行为与 procDir 这类装配参数毫无关系，造出来的 Service 只是在
// 给测试加噪音。包装一行，代价远小于那批测试的可读性收益。
func (s *Service) List(ctx context.Context, dir string, opts ListOptions) (ListPage, error) {
	return List(ctx, dir, opts)
}
