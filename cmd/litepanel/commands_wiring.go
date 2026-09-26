package main

import (
	"context"

	"litepanel/internal/api"
	"litepanel/internal/quickcmd"
	"litepanel/internal/store"
	"litepanel/internal/terminal"
)

// commands 把 quickcmd 的包级存储函数与注入器拼成 api.Commands。
//
// 为什么要有这一层：quickcmd 的存储侧沿用包级函数（与 terminal/service 的
// 存储层同形，参数第一个是 *store.DB），而 HTTP 侧要的是一个方法集。
// 这层只做转发，一条业务判断都不加 —— 加了就有了第二个校验/判定主人。
//
// 装配集中在 attachTerminalDeps：会话 CRUD 与快捷命令必须**一起**接。
// 分开接会产生"列表能看、点就 501"这种面板上一处报错都没有的半接线状态。
type commands struct {
	db  *store.DB
	inj *quickcmd.Injector
}

func (c commands) List(ctx context.Context) ([]quickcmd.Command, error) {
	return quickcmd.List(c.db)
}

func (c commands) Create(ctx context.Context, in quickcmd.Command) (quickcmd.Command, error) {
	return quickcmd.Create(c.db, in)
}

func (c commands) Update(ctx context.Context, id int64, in quickcmd.Command) error {
	return quickcmd.Update(c.db, id, in)
}

func (c commands) Delete(ctx context.Context, id int64) error { return quickcmd.Delete(c.db, id) }

func (c commands) Get(ctx context.Context, id int64) (quickcmd.Command, error) {
	return quickcmd.Get(c.db, id)
}

// Move 只做转发：方向到 sort 的语义归存储层（那里才看得见相邻行）。
func (c commands) Move(ctx context.Context, id int64, dir string) error {
	return quickcmd.Move(c.db, id, dir)
}

func (c commands) Run(ctx context.Context, cmd quickcmd.Command) (quickcmd.Result, error) {
	return c.inj.Run(ctx, cmd)
}

func (c commands) Busy(ctx context.Context, ids []int64) (map[int64]quickcmd.BusyInfo, error) {
	return c.inj.Busy(ctx, ids)
}

var _ api.Commands = commands{}

// attachTerminalDeps 把终端与依赖终端的模块接到 HTTP 依赖上。
//
// busy 窗口与注入器共用同一个值：quickcmd.DefaultBusyWindow（设置页将来
// 改的是同一个配置项）。展示与注入必须读同一个窗口，否则标签说的和实际
// 发生的会相反，所以这里只构造一个 Injector 给两侧用。
func attachTerminalDeps(deps *api.AuthDeps, db *store.DB, tw *terminalWiring) {
	deps.TermSessions = tw.Sessions
	deps.Commands = commands{
		db:  db,
		inj: quickcmd.NewInjector(tw.Sessions, quickcmd.DefaultBusyWindow),
	}
}

var _ = terminal.DefaultBin
