package main

// 设置的"保存即生效"接线（M7-T5 第 4 步）。
//
// 这一层是整个设置功能的成败所在。前面所有工作（存储层、注册表、各子系统
// 的 setter、HTTP 接口）都可能在测试里全绿，而用户改任何一项都毫无反应 ——
// 只要这里漏接一项。而"界面上写着已保存、实际什么都没变"是本项目对设置项
// 明令禁止的失效模式，也是最难靠人眼发现的一种：界面看起来完全正常。
//
// 所以 settings_wiring_test.go 里有一条**穷举**测试：遍历注册表每一项，
// 要么 applier 认识这个键（改完之后必须能观察到真实行为变化），要么它标了
// RestartRequired。新增一项而两件事都没做，测试立刻红。这条测试是"只存不读"
// 在结构上不可能的唯一保证。

import (
	"context"
	"fmt"
	"strconv"
	"time"

	"litepanel/internal/api"
	"litepanel/internal/auth"
	"litepanel/internal/config"
	"litepanel/internal/download"
	"litepanel/internal/filemgr"
	"litepanel/internal/logx"
	"litepanel/internal/metrics"
	"litepanel/internal/service"
	"litepanel/internal/settings"
	"litepanel/internal/store"
	"litepanel/internal/terminal"
)

// settingsTargets 是热生效要推到的各个子系统。
//
// 全部用指针类型而不是接口：这一层的价值恰恰是"确实在推真实对象"，中间隔
// 一层接口就把装配层测试最关键的性质（推的是生产里那个实例）测没了。
// 允许为 nil —— 某个模块没接上时（测试里、或将来做可选模块）跳过它并说明
// 原因，而不是 panic：设置页保存成功而面板崩掉，比某项没热生效糟得多。
type settingsTargets struct {
	Collector  *metrics.Collector
	Sessions   *auth.SessionStore
	Limiter    *auth.LoginLimiter
	Files      *filemgr.Service
	Supervisor *service.Supervisor
	Download   *download.Service
}

// applySettings 读取当前设置并把每一项推到对应子系统。
//
// 一次读全量（Snapshot）而不是逐项 Get：装配层要的是"这一份值的一致视图",
// 逐项读期间用户又改了一次的话，推下去的就是半新半旧的组合，而那种状态
// 事后无法从日志里复原。
//
// 返回的错误是**第一个**失败项。不中断后续项：会话有效期写失败不该连累
// 回收站策略也推不下去 —— 能生效的尽量生效，界面上如实报告哪一项没成。
func applySettings(ctx context.Context, store *settings.Store, t settingsTargets) error {
	sn, err := store.Load(ctx)
	if err != nil {
		return fmt.Errorf("读取设置快照: %w", err)
	}
	var firstErr error
	fail := func(err error) {
		if firstErr == nil {
			firstErr = err
		}
	}

	// —— 仪表盘：采样间隔 ——
	if t.Collector != nil {
		sec := sn.Int(settings.MetricIntervalSec, settings.DefaultMetricIntervalSec)
		t.Collector.SetInterval(time.Duration(sec) * time.Second)
	}

	// —— 认证：会话有效期、登录锁定 ——
	if t.Sessions != nil {
		days := sn.Int(settings.SessionTTLDays, settings.DefaultSessionTTLDays)
		if _, err := t.Sessions.SetTTL(time.Duration(days) * 24 * time.Hour); err != nil {
			fail(fmt.Errorf("会话有效期: %w", err))
		}
	}
	if t.Limiter != nil {
		fails := sn.Int(settings.LoginMaxFails, settings.DefaultLoginMaxFails)
		mins := sn.Int(settings.LoginWindowMin, settings.DefaultLoginWindowMin)
		t.Limiter.SetPolicy(fails, time.Duration(mins)*time.Minute)
	}

	// —— 文件：回收站策略、后台任务并发 ——
	if t.Files != nil {
		// 兑底传空串而不是在这里再写一遍 ".trash"：Load 已经按 库里 > config
		// > 内置默认 解析过，走到这里还是空说明装配层没接 configDefault。
		// 在装配层再造一个默认值会**静默盖掉** config 里用户写的那个名字,
		// 而 SetTrashPolicy 报错至少会让我们如实告诉用户哪一项没生效。
		name := sn.String(settings.TrashDirName, "")
		days := sn.Int(settings.TrashRetainDays, settings.DefaultTrashRetainDays)
		if _, err := t.Files.SetTrashPolicy(ctx, name, time.Duration(days)*24*time.Hour); err != nil {
			fail(fmt.Errorf("回收站策略: %w", err))
		}
		t.Files.SetJobConcurrency(sn.Int(settings.JobConcurrencyFile, settings.DefaultJobConcurrency))
	}

	// —— 服务：日志行数、停止宽限 ——
	if t.Supervisor != nil {
		t.Supervisor.SetLogLimit(sn.Int(settings.ServiceLogLines, settings.DefaultServiceLogLines))
		grace := sn.Int(settings.StopGraceSec, settings.DefaultStopGraceSec)
		t.Supervisor.SetGrace(time.Duration(grace) * time.Second)
	}

	// —— 终端：历史行数的默认档位 ——
	// 只影响之后新建的会话（tmux 的 history-limit 在建会话时定死）。
	terminal.SetDefaultHistoryLimit(sn.Int(settings.TermHistoryLimit, settings.DefaultTermHistoryLimit))

	// —— 下载：面板默认目录/分片 + aria2 侧全局并发 ——
	if t.Download != nil {
		dir := sn.String(settings.Aria2DownloadDir, "")
		split := sn.Int(settings.Aria2Split, settings.DefaultAria2Split)
		t.Download.SetDefaults(dir, split)
		// aria2 侧的全局并发。失败不致命：面板侧的默认已经生效，
		// 这一项失败只影响"aria2 自己的排队上限"，如实报错即可。
		maxc := sn.Int(settings.Aria2MaxConcurrent, settings.DefaultAria2MaxConcurrent)
		if err := t.Download.ApplyGlobalMaxConcurrent(ctx, maxc); err != nil {
			// aria2 没装/没起不算这次保存的失败：下载页有一个**专门的**健康
			// 通道在报那件事（见 /dl/health），设置页再报一次"aria2 不可达"
			// 会把一个没装 aria2 的用户的每次保存都染成红色，而他改的采样
			// 间隔其实生效了 —— 那才是误导。真·RPC 拒绝（地址对但 aria2 报错）
			// 仍然上报，那种是"设了却没用上"，用户必须知道。
			if !download.IsUnavailable(err) {
				fail(fmt.Errorf("aria2 全局并发: %w", err))
			}
		}
	}
	// aria2_rpc_url / aria2_rpc_secret 是 RestartRequired：这里**故意**不处理,
	// 由注册表穷举测试承认它们是"有意为之"而不是"忘了"。

	return firstErr
}

// settingsDefaultsFromConfig 把 config 里与设置项重叠的键暴露给 Store 作
// 兜底（查找顺序：库里 > config > 内置默认）。
//
// 哪些键走这里、哪些不走，判据是"config.toml 里有没有对应项"：回收站、
// 并发、aria2 三组在部署期由 config 决定（install.sh 写模板），用户没在
// 设置页改过时应该继续沿用部署值。数字/会话类键 config 里没有，返回空串
// 让 Store 落到内置默认。
//
// 漏映射一个键不会报错，只会让它退回内置默认而**静默无视** config ——
// 这正是 TestSettingsConfigMapperIsConsistent 要挡的漂移。
func settingsDefaultsFromConfig(cfg config.Config) func(settings.Key) string {
	return func(k settings.Key) string {
		switch k {
		case settings.TrashDirName:
			return cfg.TrashDirName
		case settings.TrashRetainDays:
			return positiveOrEmpty(cfg.TrashRetainDays)
		case settings.JobConcurrencyFile:
			return positiveOrEmpty(cfg.JobConcurrency)
		case settings.Aria2RPCURL:
			return cfg.Aria2RPCURL
		case settings.Aria2RPCSecret:
			return cfg.Aria2RPCSecret
		case settings.Aria2DownloadDir:
			return cfg.Aria2DownloadDir
		}
		return ""
	}
}

// positiveOrEmpty：数字型的 config 项为 0（没写）时返回空串而不是 "0"。
// "0" 会被当成一个合法的 config 兜底值，从而盖掉内置默认，再被子系统夹到
// 最小值 —— 用户没写这项，却因为一个零值被拖到边界。config.Load 总会填
// 默认所以生产不会走到，这里挡的是"直接构造 config.Config{}"的测试与将来
// 的装配改动。
func positiveOrEmpty(n int) string {
	if n <= 0 {
		return ""
	}
	return strconv.Itoa(n)
}

// wireSettings 把设置模块接进路由依赖，并在启动时把库里的值推给各子系统。
//
// 为什么 boot 时必须应用一次：各子系统的构造器只看 config（newFileService
// 拿 cfg.JobConcurrency 等）。缺这一步的话，用户在设置页改过的值在面板
// **重启后就悄悄丢效果** —— 库里的值还在、GET 也显示它，子系统跑的却是
// config 里的旧值。那种"重启后自己变回去"最难被归因。
//
// 应用动作放在本函数内部而不是摊在 main 里，是为了让它可测：main 没有测试
// 抓手，而这里每一行漏接的后果都是整个设置功能变成只存不读。测试只要调
// wireSettings 再观察子系统，就能证明"启动应用"这件事真的发生了。
//
// 启动应用是**异步 + 独立超时**的：applier 会去碰 aria2（推全局并发数），
// 而 aria2 客户端有 10s 超时 —— 同步跑的话 aria2 掉线会把面板启动拖慢 10
// 秒。"面板起来了而下载稍后跟上"是用户可以接受的状态；"面板整个起不来"
// 不是（与下载启动对账同一个取舍，见 download_wiring.go 头注）。
func wireSettings(deps *api.AuthDeps, db *store.DB, cfg config.Config,
	col *metrics.Collector, sup *service.Supervisor) {
	store := settings.NewStore(db, settingsDefaultsFromConfig(cfg), nil)
	deps.Settings = store
	files, _ := deps.Files.(*filemgr.Service)
	dl, _ := deps.Downloads.(*download.Service)
	targets := settingsTargets{
		Collector:  col,
		Sessions:   deps.Sessions,
		Limiter:    deps.Limiter,
		Files:      files,
		Supervisor: sup,
		Download:   dl,
	}
	// 断言失败（装配接错类型）的后果必须响得起来：目标为 nil 会让 applier
	// 整段跳过对应模块，设置页显示"已保存"而什么都不生效 —— 与漏接 applier
	// 本身是同一个最难发现的失效模式，所以显式检查而不是依赖 nil 安全。
	if files == nil {
		logx.Error("设置接线：Files 不是 *filemgr.Service，文件类设置不会生效")
	}
	if dl == nil {
		logx.Error("设置接线：Downloads 不是 *download.Service，下载类设置不会生效")
	}
	apply := func(ctx context.Context) error {
		return applySettings(ctx, store, targets)
	}
	deps.SettingsApply = apply

	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		if err := apply(ctx); err != nil {
			// 不 fatal：能生效的项已经生效了（applier 逐项尽力，见其注释）。
			logx.Info("启动时应用已存设置未完成: %v", err)
		}
	}()
}
