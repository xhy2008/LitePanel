package filemgr

import (
	"context"
	"sync"
	"time"

	"litepanel/internal/metrics"
	"litepanel/internal/store"
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
	// db 是 fs_jobs 的落库位置（见 Options.DB）。
	db *store.DB
	// jobConcurrency / executor 见 Options 同名项。
	jobConcurrency int
	executor       jobExecutor
	// progressObserver 是进度落库次数的钩子（只有测试用）。节流是这条
	// 路径上唯一的性能护栏，而它的效果不能用"过了多久"断言（墙钟在负载
	// 高的机器上会漂），只能数次数。
	progressObserver func()
	// wakeInterval 是没有唤醒信号时的兜底轮询周期。导出给测试拉长，
	// 好把"靠 tick 也能跑"与"提交后立刻跑"区分开。
	wakeInterval time.Duration
	// wake 让新提交的任务立刻被 worker 取走；缓冲 1，非阻塞投递。
	wake chan struct{}

	// jobsWG 统计正在跑的 worker，关停时要等它们把状态写清楚。
	jobsWG sync.WaitGroup
	// runningCancel 保存每个在跑任务的取消函数（按任务 id）。
	runningMu     sync.Mutex
	runningCancel map[int64]context.CancelFunc
	// poolMu / poolStarted 保证重复 StartJobs 不起两套 worker：
	// 两套会把并发上限翻倍，而配置文件里的"并发 2"是用户唯一能看到的数字。
	poolMu sync.Mutex
	// jobsCtx 是池的父上下文，也是"关停"与"用户取消"的唯一区分依据：
	// 它被取消 = 面板在退出，任务落成 interrupted；只有任务自己的 ctx
	// 被取消 = 用户点的取消，落成 canceled。两者混成一个的话，一次正常
	// 重启就会把用户没取消过的删除记成"已取消"。
	jobsCtx     context.Context
	poolStarted bool

	// uploadRoot 是分块暂存目录（面板自己的，绝不放进用户目录）。
	uploadRoot string
	// uploadTTL 是未完成会话的闲置上限。设计 8.2 说"关浏览器不中断"，
	// 但也不能永远留着：上传一半关页面是常态，没有 TTL 用户的盘会
	// 被面板悄悄吃满。
	uploadTTL time.Duration
	// maxChunk 是单块字节上限（服务端说了算，防止一个请求体写穿磁盘）。
	maxChunk int64
	// maxUpload 是整个上传的大小上限。超过它的要走 SFTP，见 ErrTooLarge。
	maxUpload int64
	// clock 注入时间（TTL 判定用）。
	clock func() time.Time

	// trashDirName 是各盘根目录下的回收站目录名。
	trashDirName string
	// trashRetain 是回收站条目保留期（CleanTrash 用）。
	trashRetain time.Duration
	// fsRoot 查"路径属于哪个文件系统锚点"（见 Options.FilesystemRoot）。
	fsRoot rootFunc
	// trashRoots 枚举要管哪些盘（见 Options.TrashRoots）。
	trashRoots trashRootsFunc
	// sameFS 判两个路径是否同一文件系统（见 Options.SameFS）。
	sameFS sameFSFunc
	// moveHooks 只有测试用（见 moveHooks）。生产永远为 nil。
	moveHooks *moveHooks

	// uploadMu 串行化同一会话元信息的读改写。分块各写各的文件、互不
	// 相干（并发上传数默认 3），所以锁只保护 meta.json 这一小块。
	// 挂在 Service 上而不是包级变量：包级锁会把无关面板实例（测试里
	// 每个面板一个）串到同一把锁上，排查阻塞时非常误导。
	uploadMu sync.Mutex
}

// Options 是装配参数。零值可用（procDir 默认 "/proc"）。
type Options struct {
	ProcDir string
	// DB 供后台任务队列落库（M6-T4）。nil = 队列不可用，相关方法明确
	// 报错而不是静默丢任务 —— "提交成功但根本没在跑"比报错糟得多。
	DB *store.DB
	// DiskUsage 覆盖 statfs 实现（测试注入）。nil = 真实调用。
	DiskUsage func(string) (diskUsage, error)

	// UploadRoot 是分块暂存目录。空 = <db_path 同级>/uploads（装配层负责）。
	UploadRoot string
	// UploadTTL 覆盖闲置上限，零值 = DefaultUploadTTL。
	UploadTTL time.Duration
	// MaxChunkBytes 覆盖单块上限，零值 = DefaultMaxChunkBytes。
	MaxChunkBytes int64
	// MaxUploadBytes 覆盖整次上传上限，零值 = DefaultMaxUploadBytes。
	MaxUploadBytes int64
	// Clock 注入时间。nil = time.Now。
	Clock func() time.Time

	// JobConcurrency 是后台任务的并发上限，零值 = DefaultJobConcurrency
	// （设计 8.4：默认 2）。
	JobConcurrency int
	// JobExecutor 覆盖任务的实际执行体，nil = 用内置的 copy/move/delete。
	//
	// 必须有注入点：池的语义（并发上限、取消传递、关停对账、进度节流）
	// 需要一个能被卡住的任务才测得准，而真实复制在本机是几百微秒量级 ——
	// 拿真操作测池，等于只能在"睡多久"上赌时间。
	JobExecutor func(ctx context.Context, j Job, report progressFunc) error

	// TrashDirName 覆盖各盘根目录下的回收站目录名，空 = DefaultTrashDirName。
	TrashDirName string
	// TrashRetain 覆盖回收站保留期，零值 = DefaultTrashRetain。
	// 超出 [1,90] 天的值会被夹到边界（设置页负责拒绝非法值并回错误，
	// 装配层只负责不让自己进入无意义状态）。
	TrashRetain time.Duration
	// FilesystemRoot 覆盖"某个路径属于哪个文件系统锚点"的查找，
	// nil = 真实实现（向上走祖先、比较 statfs 设备号）。
	//
	// 必须有注入点：要可靠测到"删除跨盘文件绝不退化成复制"，就得有一台
	// 真的有两个可写文件系统的机器。本机的第二个盘是 sdcardfs，往里写
	// 会污染用户目录，而 CI/目标机的盘数更不确定 —— 那种测试会在某些
	// 机器上悄悄什么都不测。注入之后夹具可以任意虚构盘数。
	FilesystemRoot func(path string) (string, error)
	// JobWakeInterval 覆盖没有唤醒信号时的兜底轮询周期，零值 = 默认。
	// 只有测试会改它（拉长它才能证明"提交后立刻跑"不是靠轮询兜的）。
	JobWakeInterval time.Duration
	// TrashRoots 覆盖"要管哪些盘"（列举/清理/清空要遍历所有盘）。
	// nil = 从挂载表枚举真实盘。理由与 FilesystemRoot 同样：真实盘数
	// 取决于跑测试的机器，不注入的话"跨盘列举"会在某些机器上悄悄
	// 只覆盖一个盘，测试照样绿而什么都没测。
	TrashRoots func(ctx context.Context) ([]string, error)
	// SameFS 覆盖"两个路径是不是同一文件系统"的判定，nil = 比设备号。
	//
	// 移动的执行路径完全由这个布尔决定（同盘 rename vs copying + 校验 +
	// 删源），而这两者的失败方式相反：前者失败不会丢数据，后者做错一步
	// 就是不可逆的。必须能在**任何机器上**把两条路都跑起来测——本机只有
	// 一个可写文件系统，注入之后两条路都跑的是真文件操作，只有"是不是
	// 跨盘"这个判定来自夹具。
	SameFS func(a, b string) (bool, error)
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
	clock := opts.Clock
	if clock == nil {
		clock = time.Now
	}
	ttl := opts.UploadTTL
	if ttl <= 0 {
		ttl = DefaultUploadTTL
	}
	maxChunk := opts.MaxChunkBytes
	if maxChunk <= 0 {
		maxChunk = DefaultMaxChunkBytes
	}
	maxUpload := opts.MaxUploadBytes
	if maxUpload <= 0 {
		maxUpload = DefaultMaxUploadBytes
	}
	concurrency := opts.JobConcurrency
	if concurrency <= 0 {
		concurrency = DefaultJobConcurrency
	}
	wakeInterval := opts.JobWakeInterval
	if wakeInterval <= 0 {
		wakeInterval = defaultJobWakeInterval
	}
	trashName := opts.TrashDirName
	if trashName == "" {
		trashName = DefaultTrashDirName
	}
	retain := opts.TrashRetain
	if retain <= 0 {
		retain = DefaultTrashRetain
	}
	// 夹到边界而不是报错：NewService 没有错误返回，装配期报错等于
	// 让面板起不来。真正的"非法值回 400"在设置页那一层做（M7-T3）。
	if retain < minTrashRetain {
		retain = minTrashRetain
	}
	if retain > maxTrashRetain {
		retain = maxTrashRetain
	}
	svc := &Service{
		procDir: dir, db: opts.DB, usage: usage,
		jobConcurrency: concurrency, wakeInterval: wakeInterval,
		wake: make(chan struct{}, 1), runningCancel: map[int64]context.CancelFunc{},
		uploadRoot: opts.UploadRoot, uploadTTL: ttl,
		maxChunk: maxChunk, maxUpload: maxUpload, clock: clock,
		trashDirName: trashName, trashRetain: retain,
		fsRoot: rootFinder(opts.FilesystemRoot),
		sameFS: sameFSFinder(opts.SameFS),
	}
	// 盘的枚举要读挂载表，而挂载表的位置（procDir）在 Service 上，
	// 所以这一项只能在结构体建好之后接（不像 fsRoot 那样是个纯函数）。
	// 接不上真实实现就会漏掉"一个真盘都没枚举到时兜底 /"那段逻辑。
	// 执行器：注入的用注入的，否则用内置的 copy/move/delete。内置那套
	// 是 svc 上的方法，只能在 svc 建好之后接。
	if opts.JobExecutor != nil {
		svc.executor = opts.JobExecutor
	} else {
		svc.executor = svc.runJob
	}
	if opts.TrashRoots != nil {
		svc.trashRoots = opts.TrashRoots
	} else {
		svc.trashRoots = svc.discoverTrashRoots
	}
	return svc
}

// UploadRoot 暴露暂存根，供装配层与测试确认接的是哪一个目录。
func (s *Service) UploadRoot() string { return s.uploadRoot }

// TrashDirName 暴露回收站目录名，供装配层与测试确认配置接上了没有。
func (s *Service) TrashDirName() string { return s.trashDirName }

// TrashRetain 暴露保留期（已经过夹取），理由同上。
func (s *Service) TrashRetain() time.Duration { return s.trashRetain }

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

// JobConcurrency 回报实际生效的并发上限。
//
// 有 accessor 是因为"配置文件里写的数字有没有落到执行层"这件事，在没 accessor
// 的情况下**没有任何外部可观察面**：漏传会被默认值悄悄夹住，不报错、不改行为,
// 只让配置变成摆设。有了它，装配测试可以钉住传参本身，而不是靠"任务看起来
// 有没有变快"（并发从 2 到 4 在机械盘上肉眼不可分辨）。
func (s *Service) JobConcurrency() int { return s.jobConcurrency }
