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
	"errors"
	"fmt"
	"os/exec"
	"sort"
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
		// tmux 侧半成功残骸由 CreateSession 自己负责（见 session.go 里
		// handoff 标志那段）：这里再清一次是重复所有权，而且漏掉
		// termws 那条直接调 CreateSession 的路。
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

// List 返回元数据并对每个会话做尸检：刷新 alive 位与死因，必要时清理。
//
// 这就是死因规程的执行点（判定表在 death.go 顶部，用户 2026-09 裁决）：
// 前端每 3 秒轮询一次，正常退出的会话因此在几秒内从列表消失、不留痕迹；
// 异常的带着死因留着等手动删。
//
// 尸检取代了原来的 `tmux ls`：同样一条进程，list-panes -a 还多带每会话
// 的存活与退出码。**没有**保留 ls+list-panes 双通道 —— ListSessions 把
// "没 server"咽成"空列表+nil"（见其 noServerSays 注释），尸检必须能
// 分辨"server 没起"与"会话全没了"，否则规则 4 会退化成"全部凭空消失"。
func (s *Service) List(ctx context.Context) ([]SessionMeta, error) {
	all, err := ListSessionsMeta(s.db)
	if err != nil {
		return nil, err
	}
	statuses, err := s.tmuxStatus(ctx)
	if errors.Is(err, ErrNoServer) {
		// 「没有 server」不是"未知"，而是确凿的死：tmux 会话只存在于
		// server 内存里，不落盘 —— server 没了，名下会话必然全没。
		// （remain-on-exit 保证正常退出**不会**让 server 提前死：尸体
		// 吊着 server，直到 sweep 清掉最后一具 —— 实测。）
		//
		// 唯一心虚的情形是 socket 目录指错（TMUX_TMPDIR 配错）：会话
		// 其实活在另一个 socket 上。它与真没 server **不可区分**，所以
		// 选可恢复的那头：标"异常消失"（行与历史都留着），名字再出现
		// 时复活（sweepRow 规则 0）。反过来"什么都不动"才是不可恢复
		// 的谎报：面板永远显示一排点不开的 alive。
		if err := markAllVanished(s.db, &all); err != nil {
			return nil, err
		}
		return all, nil
	}
	if err != nil {
		// tmux 二进制都没有、或说了我们听不懂的话：这才是真"未知"，
		// 报错且一行都不动（前端显示"未知"而不是空列表）。
		return nil, err
	}
	items := make([]SessionMeta, 0, len(all))
	for i := range all {
		st, seen := statuses[all[i].TmuxName]
		deleted, err := s.sweepRow(&all[i], st, seen)
		if err != nil {
			return nil, err
		}
		if deleted {
			continue // 规则 1：正常退出的行不进列表（库里也已删）
		}
		items = append(items, all[i])
	}
	return items, nil
}

// sweepRow 尸检一行，返回 deleted（规则 1：行已删，不该再进列表）。
//
// 库写失败直接扔出去：本包没有日志系统可打，而"结构上不自洽"比
// "悄悄漏清一条"好 —— 半途失败下一轮从头重算，所有动作都以 tmux
// 现场为准，不存在需要记住的中间态。
func (s *Service) sweepRow(m *SessionMeta, st PaneStatus, seen bool) (bool, error) {
	// 规则 0：名字重新出现且 pane 活着 —— 复活。
	// 它让"socket 指错导致全体误判消失"在配置修好后自愈（见 List 里
	// ErrNoServer 那段）。死因必须一起清：留着旧退出码会让前端给一个
	// 正在跑的会话挂「异常退出 9」。
	if seen && !st.Dead && !m.Alive {
		if err := ClearSessionDeath(s.db, m.ID); err != nil {
			return false, err
		}
		m.Alive, m.ExitStatus = true, nil
		return false, nil
	}
	// 规则 1：正常退出的尸体 —— 杀尸体、删行，不留任何痕迹。
	// 先 kill 再删行：半途失败（行还在、尸体没了）下一轮按"凭空消失"
	// 记 -1 保留 —— 多一个要手动关的标签，方向正确。
	if seen && st.Dead && st.ExitStatus == 0 {
		if err := s.deleteExact(m.ID); err != nil {
			return false, err
		}
		return true, nil
	}
	if !m.Alive {
		return false, nil // 死因已记过：不再观测、也不重写
	}
	if !seen {
		// 规则 3：名字凭空消失（外部 kill-session 等）—— 异常，记 -1。
		v := ExitVanished
		if err := SetSessionDeath(s.db, m.ID, false, &v); err != nil {
			return false, err
		}
		m.Alive, m.ExitStatus = false, &v
		return false, nil
	}
	if st.Dead {
		// 规则 2：异常退出。死因在**第一次闻到尸体时**就落库：尸体
		// 比持久化短命（kill-server 连尸体一起毁），只信现场等于崩
		// 一次丢一次死因（death_test TestExitCodeSurvivesFullCrash）。
		code := st.ExitStatus
		if err := SetSessionDeath(s.db, m.ID, false, &code); err != nil {
			return false, err
		}
		m.Alive, m.ExitStatus = false, &code
		return false, nil
	}
	return false, nil // 活着且本来就是活的：不动
}

// markAllVanished 把库里所有还记着 alive 的行改判"凭空消失"。
// 幂等：只动 alive=1 的行，server 持续不可达期间的反复轮询不反复写库。
func markAllVanished(db *store.DB, all *[]SessionMeta) error {
	for i := range *all {
		m := &(*all)[i]
		if !m.Alive {
			continue
		}
		v := ExitVanished
		if err := SetSessionDeath(db, m.ID, false, &v); err != nil {
			return err
		}
		m.Alive, m.ExitStatus = false, &v
	}
	return nil
}

// tmuxStatus 拿尸检结果。区分三种世界：server 在（map 可能为空）、
// server 确定不在（ErrNoServer，见 death.go 判据）、其他故障（真未知）。
func (s *Service) tmuxStatus(ctx context.Context) (map[string]PaneStatus, error) {
	if _, err := exec.LookPath(s.bin); err != nil {
		return nil, fmt.Errorf("找不到 tmux(%s): %w", s.bin, err)
	}
	return ListPaneStatuses(s.bin, TmuxPrefix)
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
	return s.deleteExact(id)
}

// deleteExact 按 id 删会话（tmux 侧 + 库），tmux 侧用精确名。
// Delete（用户点删除）与 sweep（正常退出自动清理）共用这一份语义，
// 谁也不许自己抄一份 —— 两边对"server 已经没了"的宽容度必须一致。
func (s *Service) deleteExact(id int64) error {
	// 用 HasSession 而不是直接 kill：-t 前缀匹配语义下，一次盲杀有
	// 可能连到 lp-1 / lp-10 里的另一个。
	// 也顺带盖住"杀掉最后一个会话后 server 整个退出"：之后 kill-session
	// 会报 no server，而用户要的结果（没了）已达成。
	ok, err := HasSession(s.bin, TmuxName(id))
	if err != nil {
		return err
	}
	if ok {
		if err := KillSession(s.bin, TmuxName(id)); err != nil {
			// sweep 场景的竞态：快照说尸体还在、动手时 server 却整个
			// 没了（kill-server）。现场是被整体摧毁的，这是**异常**，
			// 不能按规则 1 当"正常退出、不留痕迹"删掉 —— 上抛 ErrNoServer，
			// 本轮 List 作废，下一轮按新现场重判（会走消失/异常分支）。
			// 用户手点的 Delete 撞上也合理：报错让他看见"没删干净"。
			if noServerSays(err.Error()) {
				return fmt.Errorf("%w: %v", ErrNoServer, err)
			}
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

// Reconcile 在面板启动时把库刷成 tmux 的样子（D5：面板重启终端不死），
// 并给每个在 tmux 里活着的 lp-* 上膛（remain-on-exit）。
//
// 上膛必须在这里做而不只在 CreateSession：
//   - 用户手工起的 lp-*（收编来的）从没经过咱们的创建路径；
//   - 面板在 tmux 之后启动（手动重启面板）时，会话早已存在。
//
// 漏上膛的后果是判错死因（正常退出变"凭空消失"），见 death_test。
// 每轮对账对所有在场会话都做（set-option 幂等、一条亚毫秒进程）：
// 比"记住哪些上过膛"少一个会腐烂的状态。
func (s *Service) Reconcile(ctx context.Context) error {
	statuses, err := s.tmuxStatus(ctx)
	if err != nil {
		return err
	}
	names := make([]string, 0, len(statuses))
	for name := range statuses {
		names = append(names, name)
	}
	if _, err := ReconcileMeta(s.db, func() ([]string, error) { return names, nil }); err != nil {
		return err
	}
	// 名字取 tmux 现场而非库：库里有死尸行，给尸体上膛没意义。
	// 排序让错误信息可预期（map 遍历随机）。
	sort.Strings(names)
	for _, name := range names {
		if err := ArmCorpse(ctx, s.bin, name); err != nil {
			return err
		}
	}
	return nil
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

// 编译期钉子：Service 必须满足 API 层声明的 TermSessions 接口。
// 没有它，"API 测试用替身全绿、真实现签名对不上"会一路拖到装配层才炸。
var _ interface {
	Create(context.Context, SessionInput) (SessionMeta, error)
	List(context.Context) ([]SessionMeta, error)
	Rename(context.Context, int64, string) error
	Delete(context.Context, int64) error
} = (*Service)(nil)
