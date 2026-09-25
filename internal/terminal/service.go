package terminal

// 终端会话服务：把 term_sessions 表（元数据）与 tmux（真相）拼成一个
// API 能直接用的门面。
//
// 谁是主人这件事必须说死，否则两边会各自漂移：
//   - **tmux 是"会话在不在"的唯一真相**。库里 alive 只是一个缓存，
//     由 Reconcile 从 tmux 刷新；面板 crash、用户在 tmux 里 exit、
//     手工 kill-server 都不会让库里的说法变成事实。
//   - **库是"这个会话叫什么、当初怎么建的"的唯一真相**。tmux 只知道
//     lp-17 这个名字，不知道它叫"跑备份"。
//
// 因此 Create 的失败处理是刻意的：先建 tmux 会话、成功之后才落库。反过来
// 会在库里留下一堆 tmux 里根本不存在的行，而列表页以库为准 —— 用户会看到
// 一排点不开的僵尸会话。

import (
	"context"
	"fmt"
	"os/exec"
	"time"

	"litepanel/internal/store"
)

// OpenControl 打开一条 control 连接（桥接层用它接管会话）。
type OpenControl func(id int64) (*Session, error)

// Service 是终端会话的门面。
type Service struct {
	db  *store.DB
	bin string
}

// NewService 建会话服务。bin 是 tmux 可执行文件路径。
func NewService(db *store.DB, bin string) *Service {
	if bin == "" {
		bin = DefaultBin
	}
	return &Service{db: db, bin: bin}
}

// Create 建会话。
//
// 顺序上是"库拿 id → tmux 用该 id 建会话 → 失败则回滚库里的行"：
// tmux 会话名必须是 lp-<id>，而 id 只有插完库才知道，所以没法先建 tmux。
// 反过来想（先 tmux 后库）会撞 tmux_name UNIQUE 与 id 的鸡生蛋问题。
// 真正要防的是"库里留一行 tmux 里不存在的僵尸"，那由失败分支的
// DeleteSessionMeta 负责。
func (s *Service) Create(ctx context.Context, in SessionInput) (SessionMeta, error) {
	meta, err := CreateSessionMeta(s.db, in)
	if err != nil {
		return SessionMeta{}, err
	}
	sess, err := CreateSession(s.bin, meta.TmuxName, SessionOpts{
		Cols: DefaultCols, Rows: DefaultRows,
		Cwd: meta.Cwd, Shell: meta.Shell, HistoryLimit: meta.HistoryLimit,
	})
	if err != nil {
		// tmux 没建起来 → 库里的行是纯噪音，立刻收回
		_ = DeleteSessionMeta(s.db, meta.ID)
		return SessionMeta{}, fmt.Errorf("创建 tmux 会话: %w", err)
	}
	// 用完必须关：CreateSession 顺带 attach 了一条控制连接，而 REST 这条路上
	// 没有人会读它的 Events —— 攥着不放等于每次创建泄漏一个 tmux 客户端进程，
	// 且其缓冲填满后解码协程永久阻塞。扇出由桥接在浏览器订阅时自己 attach。
	_ = sess.Close()
	return meta, nil
}

// 新建会话的初始网格。真实尺寸由第一个接入的设备上报（见 termws），
// 这里只是给"还没人接入时"一个不出格的初值。
const (
	DefaultCols = 100
	DefaultRows = 28
)

// List 返回元数据并把 alive 刷新成 tmux 的实际情况。
//
// 每次列表都问一次 tmux 而不是信库里的缓存：`tmux ls` 是亚毫秒级的，
// 而"面板说在、点进去说没这个会话"是最让人不信任的错法。
func (s *Service) List(ctx context.Context) ([]SessionMeta, error) {
	names, err := s.tmuxSessions(ctx)
	if err != nil {
		// tmux 整个问不到（没装 / server 没起）时不谎报"全部还在"，
		// 也不谎报"全没了"：直接报错，让 API 回 5xx。
		return nil, err
	}
	live := make(map[int64]bool, len(names))
	for _, n := range names {
		if id, ok := SessionIDFromTmuxName(n); ok {
			live[id] = true
		}
	}
	items, err := ListSessionsMeta(s.db)
	if err != nil {
		return nil, err
	}
	for i := range items {
		alive := live[items[i].ID]
		if alive != items[i].Alive {
			if err := setSessionAlive(s.db, items[i].ID, alive); err != nil {
				return nil, err
			}
			items[i].Alive = alive
		}
	}
	return items, nil
}

// Rename 改标题。
func (s *Service) Rename(ctx context.Context, id int64, title string) error {
	return RenameSessionMeta(s.db, id, title)
}

// Delete 删会话：先确认存在，杀 tmux 会话，再删库里的行。
//
// tmux 会话已经不存在不算失败 —— 用户要的结果（没了）已经达成，
// 把库里的僵尸行删掉正是他想要的收尾。
func (s *Service) Delete(ctx context.Context, id int64) error {
	if _, err := GetSessionMeta(s.db, id); err != nil {
		return err
	}
	// 先问 tmux 在不在：杀掉最后一个会话后 server 会整个退出，之后的
	// kill-session 会报 "no server running"。用户要的结果（这行没了）
	// 已经达成，不该因为一条迟到的报错而失败。
	// 用 HasSession 而不是直接 kill：-t 的前缀匹配语义下，
	// 一次盲杀有可能连到 lp-1 / lp-10 里的另一个。
	ok, err := HasSession(s.bin, TmuxName(id))
	if err != nil {
		return err
	}
	if ok {
		if err := KillSession(s.bin, TmuxName(id)); err != nil {
			return err
		}
	}
	return DeleteSessionMeta(s.db, id)
}

// Attach 为桥接层打开 control 连接。
func (s *Service) Attach(ctx context.Context, id int64) (*Session, error) {
	if _, err := GetSessionMeta(s.db, id); err != nil {
		return nil, err
	}
	if err := TouchSessionMeta(s.db, id, time.Now().Unix()); err != nil {
		return nil, err
	}
	return Attach(s.bin, TmuxName(id))
}

// Reconcile 在面板启动时把库刷成 tmux 的样子（D5：面板重启终端不死）。
func (s *Service) Reconcile(ctx context.Context) error {
	_, err := ReconcileMeta(s.db, func() ([]string, error) { return s.tmuxSessions(ctx) })
	return err
}

// OpenControl 给桥接层的工厂：桥接只需要"按 id 拿到会话"，
// 不需要知道数据库、路径前缀这些细节。
func (s *Service) OpenControl() OpenControl {
	return func(id int64) (*Session, error) { return s.Attach(context.Background(), id) }
}

func (s *Service) tmuxSessions(ctx context.Context) ([]string, error) {
	if _, err := exec.LookPath(s.bin); err != nil {
		// LookPath 失败时 ListSessions 会回非零退出 + "no server running"，
		// 那是"没会话"而不是"坏了"。这里显式区分，避免把没装 tmux
		// 当成一次普通查询失败。
		return nil, fmt.Errorf("找不到 tmux(%s): %w", s.bin, err)
	}
	return ListSessions(s.bin, TmuxPrefix)
}

// setSessionAlive 单独刷 alive 位（对账与列表共用语义：tmux 说在就是在）。
func setSessionAlive(db *store.DB, id int64, alive bool) error {
	n := 0
	if alive {
		n = 1
	}
	_, err := db.SqlDB().Exec(`UPDATE term_sessions SET alive=? WHERE id=?`, n, id)
	return err
}

// 编译期钉子：Service 必须满足 API 层声明的 TermSessions 接口。
// 没有它，"API 测试用替身全绿、真实现签名对不上"会一路拖到装配层才炸。
var _ interface {
	Create(context.Context, SessionInput) (SessionMeta, error)
	List(context.Context) ([]SessionMeta, error)
	Rename(context.Context, int64, string) error
	Delete(context.Context, int64) error
} = (*Service)(nil)
