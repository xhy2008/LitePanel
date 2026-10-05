// Package settings 存放"用户能在设置页改、且后端真的会读"的运行期设置。
//
// 与 internal/config 的分工：config 是**启动时**读的配置文件（改完要重启），
// settings 是**运行中**由设置页写的库表（改完立即生效）。两边的默认值以
// config.Defaults() 为准，这里不再抄一份数字 —— 抄一份就会有"改了配置默认
// 值、设置页显示的还是旧的"这种两处不一致。
//
// 为什么用"已知键 + 类型 + 取值范围"的注册表，而不是 map[string]string
// 随便存（表结构本身就是 key/value）：
//   - 只存不读的设置项比没有更糟。界面告诉用户"已保存"，而他改的东西没
//     有任何作用，他只能一直用那个他一直以为已经改掉的值。注册表逼着每个
//     键在这里出现一次，而"它在哪生效"由装配层测试钉住（见
//     cmd/litepanel/settings_wiring_test.go）。
//   - 取值范围必须写在类型上而不是 handler 里：同一个键会被 GET 展示、被
//     PUT 校验、被装配层读取，三处各写一遍边界必然漂移。
package settings

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/url"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"litepanel/internal/store"
)

// Key 是一个已知设置项的标识（也是库里的 key 列）。
type Key string

const (
	// —— 仪表盘 ——
	// MetricIntervalSec 是指标采样间隔秒数（1/2/5，设计 17 节"仪表盘"）。
	MetricIntervalSec Key = "metric_interval_sec"

	// —— 认证 ——
	// SessionTTLDays 是登录会话有效期（天）。
	SessionTTLDays Key = "session_ttl_days"
	// LoginMaxFails / LoginWindowMin 是登录失败锁定阈值与窗口（auth.Limiter）。
	LoginMaxFails  Key = "login_max_fails"
	LoginWindowMin Key = "login_window_min"

	// —— 文件 ——
	// TrashDirName 是各盘根目录下回收站的目录名（与 config 同名键对应）。
	TrashDirName Key = "trash_dir_name"
	// TrashRetainDays 回收利用保留天数（1–90）。
	TrashRetainDays Key = "trash_retain_days"
	// JobConcurrencyFile 是文件后台任务的并发上限（1–16）。
	JobConcurrencyFile Key = "job_concurrency"

	// —— 服务托管 ——
	// ServiceLogLines 是每服务内存日志环形缓冲的行数（D19：默认 500）。
	ServiceLogLines Key = "service_log_lines"
	// StopGraceSec 是停止托管服务时等优雅退出的秒数。
	StopGraceSec Key = "stop_grace_sec"

	// —— 终端 ——
	// TermHistoryLimit 是 tmux history-limit（5000/20000/100000 三档）。
	TermHistoryLimit Key = "term_history_limit"

	// —— 下载 ——
	// Aria2RPCURL / Aria2RPCSecret 与 config 同名键对应：库里**没有**值时
	// 取配置文件里的（配置文件是部署时生成的，设置页是事后微调）。
	Aria2RPCURL    Key = "aria2_rpc_url"
	Aria2RPCSecret Key = "aria2_rpc_secret"
	// Aria2DownloadDir 是新建下载的默认保存目录。
	Aria2DownloadDir Key = "aria2_download_dir"
	// Aria2MaxConcurrent 是一次提交给 aria2 的最大并发任务数。
	Aria2MaxConcurrent Key = "aria2_max_concurrent"
	// Aria2Split 是单任务默认分片数（2–64）。
	Aria2Split Key = "aria2_split"
)

// 各项默认值。这里放的是"库里没写过任何东西时"的值；
// 与 config 有重叠的键（回收站、并发、aria2）由装配层用 config 兜底，
// 所以那些键的默认值在这里是 zero，见 Def 的 ConfigFallback 说明。
const (
	DefaultMetricIntervalSec  = 1
	DefaultSessionTTLDays     = 30
	DefaultLoginMaxFails      = 5
	DefaultLoginWindowMin     = 10
	DefaultTrashRetainDays    = 3
	DefaultJobConcurrency     = 2
	DefaultServiceLogLines    = 500
	DefaultStopGraceSec       = 10
	DefaultTermHistoryLimit   = 20000
	DefaultAria2MaxConcurrent = 5
	DefaultAria2Split         = 5
)

// Kind 决定 GET 时怎么序列化、PUT 时怎么解析与校验。
//
// 用字符串而不是 iota 枚举：encoding/json 把命名 int 类型编码成**数字**
// （实测 KindEnum 编成 2，见下方历史说明），前端只能硬编码 0/1/2 来分支
// 表单控件（数字框 / 下拉 / 文本框）。之后在中间插一个新 Kind，前后端就
// 静默错位（旧的 1 变成新类型的语义）。字符串常量错位不了，且 GET 响应
// 自带可读性。曾经写过 Kind.String() 想绕开这点，但 json.Marshal 对
// 命名整数类型根本不调 Stringer，那是段不会被执行的死代码，已删。
type Kind string

const (
	KindInt  Kind = "int"    // 带范围限制的整数
	KindStr  Kind = "string" // 字符串（可带校验器）
	KindEnum Kind = "enum"   // 整数枚举（只能取 Enum 里列出的值）
)

// Def 描述一个已知设置项。
type Def struct {
	Key      Key
	Group    string
	Kind     Kind
	Label    string // 设置页显示的中文名
	Unit     string // 单位后缀（秒/天/行…），供前端直接拼
	Min, Max int    // KindInt 的闭区间
	Enum     []int  // KindEnum 的允许值
	// Default 是库里没写过时的取值；空串表示"没有内置默认，由装配层从
	// config 兜底"（见 DefaultOf）。
	Default string
	// Secret 标记"值不该原样出现在 GET 响应里"（aria2 的 RPC 密钥）。
	Secret bool
	// RestartRequired = 改了只能重启才生效，设置页要如实打上"重启生效"标。
	//
	// aria2_rpc_url / aria2_rpc_secret 标它是**领域事实**而不是偷懒：这两个值
	// 只在 aria2 自己的启动参数里变（aria2 由 systemd 常驻、面板不拉起它，
	// 见 D16）。改端点必然要改 aria2 的命令行并重启 aria2 —— 面板这边就算
	// 热重连了一个新地址，aria2 没在新地址监听也是白搭。所以"改完重启"不是
	// 面板的限制，是这件事本身的性质。
	//
	// 这个字段存在的唯一理由是**让"只存不读"在结构上不可能**：装配层测试
	// 遍历注册表，要求每一项要么是 applier 认识的键（保存后立刻推给子系统）,
	// 要么 RestartRequired=true。新增一项而忘了这两件事之一，测试立刻红。
	// 没有这个标记时，"忘了接"与"确实需要重启"在代码里长得一模一样。
	RestartRequired bool
	// Validate 做 Kind 之外的额外校验（回收站目录名必须是单个路径段）。
	Validate func(string) error
}

// registry 是所有已知设置项。新增一项必须同时在这里登记、在装配层接上生效
// 路径、并在 settings_wiring_test.go 里加一条"改它之后行为变了"的测试。
var registry = []Def{
	{Key: MetricIntervalSec, Group: "dashboard", Kind: KindEnum, Label: "采样间隔",
		Unit: "秒", Enum: []int{1, 2, 5}, Default: strconv.Itoa(DefaultMetricIntervalSec)},

	{Key: SessionTTLDays, Group: "auth", Kind: KindInt, Label: "登录会话有效期",
		Unit: "天", Min: 1, Max: 365, Default: strconv.Itoa(DefaultSessionTTLDays)},
	{Key: LoginMaxFails, Group: "auth", Kind: KindInt, Label: "登录失败锁定阈值",
		Unit: "次", Min: 3, Max: 100, Default: strconv.Itoa(DefaultLoginMaxFails)},
	{Key: LoginWindowMin, Group: "auth", Kind: KindInt, Label: "登录失败统计窗口",
		Unit: "分钟", Min: 1, Max: 1440, Default: strconv.Itoa(DefaultLoginWindowMin)},

	{Key: TrashDirName, Group: "files", Kind: KindStr, Label: "回收站目录名",
		Validate: validateTrashName},
	{Key: TrashRetainDays, Group: "files", Kind: KindInt, Label: "回收站保留天数",
		Unit: "天", Min: 1, Max: 90},
	{Key: JobConcurrencyFile, Group: "files", Kind: KindInt, Label: "后台任务并发数",
		Unit: "个", Min: 1, Max: 16},

	{Key: ServiceLogLines, Group: "services", Kind: KindInt, Label: "服务日志缓存行数",
		Unit: "行", Min: 50, Max: 5000, Default: strconv.Itoa(DefaultServiceLogLines)},
	{Key: StopGraceSec, Group: "services", Kind: KindInt, Label: "停止宽限时长",
		Unit: "秒", Min: 1, Max: 120, Default: strconv.Itoa(DefaultStopGraceSec)},

	{Key: TermHistoryLimit, Group: "terminal", Kind: KindEnum, Label: "回滚缓冲行数",
		Unit: "行", Enum: []int{5000, 20000, 100000}, Default: strconv.Itoa(DefaultTermHistoryLimit)},

	{Key: Aria2RPCURL, Group: "download", Kind: KindStr, Label: "aria2 RPC 地址",
		Validate: validateRPCURL, RestartRequired: true},
	{Key: Aria2RPCSecret, Group: "download", Kind: KindStr, Label: "aria2 RPC 密钥",
		Secret: true, RestartRequired: true},
	{Key: Aria2DownloadDir, Group: "download", Kind: KindStr, Label: "默认下载目录"},
	{Key: Aria2MaxConcurrent, Group: "download", Kind: KindInt, Label: "最大并发任务数",
		Unit: "个", Min: 1, Max: 20, Default: strconv.Itoa(DefaultAria2MaxConcurrent)},
	{Key: Aria2Split, Group: "download", Kind: KindInt, Label: "单任务分片数",
		Unit: "个", Min: 1, Max: 16, Default: strconv.Itoa(DefaultAria2Split)},
}

// Defs 返回全部已知项的副本（按 key 排序，保证 GET 的顺序稳定）。
func Defs() []Def {
	out := make([]Def, len(registry))
	copy(out, registry)
	sort.Slice(out, func(i, j int) bool { return out[i].Key < out[j].Key })
	return out
}

// lookup 找到某个键的定义。
func lookup(k Key) (Def, bool) {
	for _, d := range registry {
		if d.Key == k {
			return d, true
		}
	}
	return Def{}, false
}

// ErrUnknownKey 是未知键的错误（PUT 必须拒绝，见 Store.Apply 的注释）。
var ErrUnknownKey = errors.New("未知的设置项")

// Store 读写 settings 表。
type Store struct {
	db *store.DB
	// configDefault 给"库里没写过"的键兜底（装配层传 config 进来）。
	// nil = 只用 Def.Default。
	configDefault func(Key) string
	now           func() time.Time
}

// NewStore 创建设置存储。configDefault 可为 nil。
func NewStore(db *store.DB, configDefault func(Key) string, now func() time.Time) *Store {
	if now == nil {
		now = time.Now
	}
	return &Store{db: db, configDefault: configDefault, now: now}
}

// Value 是某一项的当前值（库里没有时回默认）。
type Value struct {
	Def
	// Value 是解析后的字符串值（已含默认值兜底）。
	Value string
	// Overridden 为真表示这个值来自库里（用户改过），而不是默认。
	Overridden bool
	// Set 为真表示这项**有非空值**。它就是"值非空"的派生而不是第三种状态：
	// 用户把可选字符串项（如默认下载目录）显式清空，与从没填过，对前端是
	// 同一件事 —— 都该显示"未设置"。若把"存过空串"算成 Set=true，输入框
	// 看着像已保存了一个值而它其实是空的。
	Set bool
}

// Int 读整数值（调用方确定这项是数字时用）。
func (v Value) Int() int {
	n, _ := strconv.Atoi(strings.TrimSpace(v.Value))
	return n
}

// All 返回全量设置（含未改过的，取默认值）。GET /api/settings 直接映射它。
func (s *Store) All(ctx context.Context) ([]Value, error) {
	stored, err := s.raw(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]Value, 0, len(registry))
	for _, d := range Defs() {
		v := Value{Def: d}
		if raw, ok := stored[string(d.Key)]; ok {
			v.Value = raw
			v.Overridden = true
		} else {
			v.Value = s.fallback(d.Key, d.Default)
		}
		v.Set = v.Value != ""
		out = append(out, v)
	}
	return out, nil
}

// Get 读单项。未知键返回 ErrUnknownKey。
func (s *Store) Get(ctx context.Context, k Key) (Value, error) {
	all, err := s.All(ctx)
	if err != nil {
		return Value{}, err
	}
	for _, v := range all {
		if v.Key == k {
			return v, nil
		}
	}
	return Value{}, fmt.Errorf("%w: %s", ErrUnknownKey, k)
}

// fallback 取库里没有时的值：先问装配层注入的 config，再取 Def.Default。
func (s *Store) fallback(k Key, def string) string {
	if s.configDefault != nil {
		if v := s.configDefault(k); v != "" {
			return v
		}
	}
	return def
}

// raw 读库里已存的原始键值。
func (s *Store) raw(ctx context.Context) (map[string]string, error) {
	rows, err := s.db.SqlDB().QueryContext(ctx, `SELECT key, value FROM settings`)
	if err != nil {
		return nil, fmt.Errorf("读取设置: %w", err)
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var k, v string
		if err := rows.Scan(&k, &v); err != nil {
			return nil, fmt.Errorf("读取设置: %w", err)
		}
		out[k] = v
	}
	return out, rows.Err()
}

// Apply 校验并写入一批设置，返回应用后的全量值。
//
// **未知键直接拒绝而不是忽略**：设置页发来的 key 是我们自己注册的，出现
// 没登记的键只能是前端写错或有人在直接调 API。忽略的后果是"界面显示已
// 保存、实际什么都没生效"，而这正是只存不读的设置项最难发现的一种失效。
// 一批里有一项非法就整批不写（返回第一个错误），否则用户会看到"部分生效"
// 这种更难排查的状态。
func (s *Store) Apply(ctx context.Context, in map[string]json.RawMessage) ([]Value, error) {
	if len(in) == 0 {
		return s.All(ctx)
	}
	// 先全部校验，再一次性写。
	type pending struct {
		key   Key
		value string
	}
	var todo []pending
	for _, d := range Defs() { // 按注册表顺序遍历：错误信息稳定可测
		raw, ok := in[string(d.Key)]
		if !ok {
			continue
		}
		val, err := parse(d, raw)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", d.Label, err)
		}
		todo = append(todo, pending{d.Key, val})
	}
	// in 里出现而注册表没有的键 —— 排序后报错，保证信息稳定。
	var unknown []string
	for k := range in {
		if _, ok := lookup(Key(k)); !ok {
			unknown = append(unknown, k)
		}
	}
	if len(unknown) > 0 {
		sort.Strings(unknown)
		return nil, fmt.Errorf("%w: %s", ErrUnknownKey, strings.Join(unknown, ", "))
	}
	if len(todo) == 0 {
		return s.All(ctx)
	}
	now := s.now().Unix()
	tx, err := s.db.SqlDB().BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("开始写设置: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	for _, p := range todo {
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO settings(key,value,updated_at) VALUES(?,?,?)
			 ON CONFLICT(key) DO UPDATE SET value=excluded.value, updated_at=excluded.updated_at`,
			string(p.key), p.value, now); err != nil {
			return nil, fmt.Errorf("写入设置 %s: %w", p.key, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("提交设置: %w", err)
	}
	return s.All(ctx)
}

// parse 按 Kind 校验并转成存库的字符串。
//
// 存的是**规范化的十进制字符串**而不是原始 JSON：数字项统一去空格与
// 进制前缀，字符串项保留原样，这样 All() 读回来的字符串与 PUT 进去的
// 语义一致，装配层也只需 Atoi。
func parse(d Def, raw json.RawMessage) (string, error) {
	switch d.Kind {
	case KindInt, KindEnum:
		var n int
		// 只接受 JSON 数字；"5" 这种字符串形式的数字**也**接受：表单控件
		// 发字符串是常态，而拒它的理由（防注入）不成立——数字本来就要过范围。
		if err := json.Unmarshal(raw, &n); err != nil {
			var s string
			if err2 := json.Unmarshal(raw, &s); err2 != nil {
				return "", errors.New("需要是数字")
			}
			parsed, err2 := strconv.Atoi(strings.TrimSpace(s))
			if err2 != nil {
				return "", errors.New("需要是数字")
			}
			n = parsed
		}
		if d.Kind == KindEnum {
			for _, ok := range d.Enum {
				if n == ok {
					return strconv.Itoa(n), nil
				}
			}
			return "", fmt.Errorf("只能是 %v 之一", d.Enum)
		}
		if n < d.Min || n > d.Max {
			return "", fmt.Errorf("应在 %d–%d 之间，当前 %d", d.Min, d.Max, n)
		}
		return strconv.Itoa(n), nil

	case KindStr:
		var s string
		if err := json.Unmarshal(raw, &s); err != nil {
			return "", errors.New("需要是字符串")
		}
		s = strings.TrimSpace(s)
		if d.Validate != nil {
			if err := d.Validate(s); err != nil {
				return "", err
			}
		}
		return s, nil
	}
	return "", fmt.Errorf("内部错误：未知设置类型 %q", d.Kind)
}

// Snapshot 是一次读取的设置集合，供装配层取值（比逐个 Get 少 N 次查询）。
//
// 存的是**生效值**而不是库里原样：查找顺序仍是 库里 > config > 内置默认，
// 与 All() 完全一致。这一点关键 —— 装配层如果自己再兑一遍 config 与内置
// 默认，就出现了"兑底顺序"的第二个主人；两处一旦漂移，就会 GET 显示一个
// 值而子系统按另一个值跑，而这种不一致从任何一个侧面看都"正常"。
type Snapshot struct {
	m map[string]string
}

// Load 读一份快照，每一项都是按 DB > config > 内置默认解析后的生效值。
func (s *Store) Load(ctx context.Context) (*Snapshot, error) {
	raw, err := s.raw(ctx)
	if err != nil {
		return nil, err
	}
	eff := make(map[string]string, len(raw)+len(registry))
	for _, d := range registry {
		if v, ok := raw[string(d.Key)]; ok {
			eff[string(d.Key)] = v
			continue
		}
		// 与 All() 共用同一个 fallback：库里没有 -> 问 config -> 内置默认。
		if v := s.fallback(d.Key, d.Default); v != "" {
			eff[string(d.Key)] = v
		}
	}
	return &Snapshot{m: eff}, nil
}

// String 取字符串值，没有则回 def。
func (sn *Snapshot) String(k Key, def string) string {
	if v, ok := sn.m[string(k)]; ok {
		return v
	}
	return def
}

// Int 取整数值，解析失败或没有则回 def。
//
// 坏值回默认而不是报错：这里的输入是库里已有的字符串，它可能是**旧版本
// 二进制**写下的、或者校验规则收紧前写下的。为这个让面板起不来不值得 ——
// 设置页仍然会在展示时把它按规则拒绝掉，用户能改回来；而启动失败会让
// 用户连改的机会都没有。
func (sn *Snapshot) Int(k Key, def int) int {
	raw, ok := sn.m[string(k)]
	if !ok {
		return def
	}
	n, err := strconv.Atoi(strings.TrimSpace(raw))
	if err != nil {
		return def
	}
	return n
}

// validateTrashName 与 config.validateTrash 同源：回收站建在每个盘根下，
// 所以只能是单个路径段。绝对路径/多段路径拼出来是 <盘根>/DISK/.trash，
// 会在每个盘上悄悄建目录，而界面显示的回收站跟磁盘上的对不上。
func validateTrashName(s string) error {
	if s == "" {
		return errors.New("回收站目录名不能为空")
	}
	if s != filepath.Base(s) || s == "." || s == ".." {
		return fmt.Errorf("回收站目录名必须是单个目录名（如 .trash），不能是路径：%q", s)
	}
	return nil
}

// validateRPCURL 与 config.validateAria2 同源。空串放行：库里留空表示
// "用配置文件里的地址"，那是常态而不是错误。
func validateRPCURL(s string) error {
	if s == "" {
		return nil
	}
	u, err := url.Parse(s)
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
		return fmt.Errorf("%q 不是合法 RPC 地址，应形如 http://127.0.0.1:6800/jsonrpc", s)
	}
	if u.Scheme == "http" && !isLoopbackHost(u.Hostname()) {
		return fmt.Errorf("不能对非回环地址用明文 http：%s；rpc-secret 会在网络上裸奔，"+
			"跨机请改用 https 或先拉隧道再连 127.0.0.1", s)
	}
	return nil
}

// isLoopbackHost 与 config 中同名函数一致：用 net 解析而不是比字符串前缀
// （否则 127.0.0.1.evil.com 会被当成回环）。
func isLoopbackHost(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}
