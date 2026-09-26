package quickcmd

// 快捷命令的存储与入参校验（quick_commands 表，0004 迁移）。
//
// 这一层只管"这条命令长什么样、存进去、读回来"。执行在 injector.go，
// HTTP 层的职责只是把这里的错误映射成状态码 —— 校验只在 normalized() 一处，
// 两处各写一套迟早漂开（终端会话那边同样是这个分法）。

import (
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"litepanel/internal/store"
)

var (
	// ErrNameRequired 名称必填。导出给 HTTP 层用 errors.Is 判断，
	// 而不是对 err.Error() 做中文子串匹配（改文案会把 400 变成 500）。
	ErrNameRequired = errors.New("命令名称不能为空")
	// ErrCommandRequired 命令内容必填。
	ErrCommandRequired = errors.New("命令内容不能为空")
	// ErrNotFound 指定 id 不存在。
	ErrNotFound = errors.New("快捷命令不存在")
)

// Command 是一条快捷命令。JSON tag 与 Go 侧字段同名 snake_case，
// 直接照搬结构体标签，不做 camelCase 转换（全项目一致）。
type Command struct {
	ID          int64  `json:"id"`
	Name        string `json:"name"`
	Command     string `json:"command"`
	Cwd         string `json:"cwd"`
	NeedConfirm bool   `json:"need_confirm"`
	Sort        int    `json:"sort"`
	CreatedAt   int64  `json:"created_at"`
}

// dangerousPatterns 是需要二次确认的命令特征。
//
// 匹配方式刻意是"整条命令里出现这个子串"而不是"第一个词是不是这个命令"：
// 后者要正确回答就得解析管道、sudo、命令替换和参数位置，而这里每误判一次
// 的代价是真执行一条危险命令，多弹一次确认的代价只是一次点击。判定保守
// 是设计明写的（5.3）。
//
// 已知会多弹的写法：systemctl status nginx（命中 "systemctl s"？不会 ——
// 特征串是带空格的完整动词，见下）。
var dangerousPatterns = []string{
	"rm -rf", "rm -fr", "mkfs", "dd if=", "shutdown", "reboot", "halt",
	"systemctl stop", "systemctl restart", "systemctl disable", "poweroff",
	":(){", "> /dev/sd", "chmod -R 777 /", "> /etc/",
}

// normalized 清空白、填默认值。
//
// need_confirm 是"或"上去的，不受理显式取消：这个开关防的就是手滑和
// "当时没多想"，能关掉等于没有。
func (c Command) normalized() (Command, error) {
	out := c
	out.Name = strings.TrimSpace(c.Name)
	out.Command = strings.TrimSpace(strings.TrimRight(c.Command, "\r\n"))
	out.Cwd = strings.TrimSpace(c.Cwd)
	if out.Name == "" {
		return c, ErrNameRequired
	}
	if out.Command == "" {
		return c, ErrCommandRequired
	}
	low := strings.ToLower(out.Command)
	for _, p := range dangerousPatterns {
		if strings.Contains(low, strings.ToLower(p)) {
			out.NeedConfirm = true
			break
		}
	}
	return out, nil
}

// Create 校验后插入，返回带 id 的完整行。
func Create(db *store.DB, in Command) (Command, error) {
	c, err := in.normalized()
	if err != nil {
		return Command{}, err
	}
	c.CreatedAt = time.Now().Unix()
	// sort 取"当前最大 + 10"：新命令排最后，且留出插队空间。
	// 用 +1 的话任何一次"插到中间"都要重排全表。
	c.Sort = 0
	if err := db.SqlDB().QueryRow(
		`SELECT COALESCE(MAX(sort),0)+10 FROM quick_commands`).Scan(&c.Sort); err != nil {
		return Command{}, fmt.Errorf("读排序位置: %w", err)
	}
	res, err := db.SqlDB().Exec(
		`INSERT INTO quick_commands(name,command,cwd,need_confirm,sort,created_at)
		 VALUES(?,?,?,?,?,?)`,
		c.Name, c.Command, c.Cwd, b2i(c.NeedConfirm), c.Sort, c.CreatedAt)
	if err != nil {
		return Command{}, fmt.Errorf("写入快捷命令: %w", err)
	}
	c.ID, _ = res.LastInsertId()
	return c, nil
}

// List 按用户排的顺序返回。
//
// ORDER BY sort,id 而不是 id：sort 允许重复（Move 只做局部调整），
// 没有 id 兜底的话同一批 sort 的相对顺序由 SQLite 随口决定，
// 用户会看到列表顺序在重启后自己变了。
func List(db *store.DB) ([]Command, error) {
	rows, err := db.SqlDB().Query(
		`SELECT id,name,command,cwd,need_confirm,sort,created_at
		 FROM quick_commands ORDER BY sort ASC, id ASC`)
	if err != nil {
		return nil, fmt.Errorf("读快捷命令: %w", err)
	}
	defer rows.Close()
	var out []Command
	for rows.Next() {
		var (
			c  Command
			nc int
		)
		if err := rows.Scan(&c.ID, &c.Name, &c.Command, &c.Cwd, &nc, &c.Sort, &c.CreatedAt); err != nil {
			return nil, fmt.Errorf("读快捷命令: %w", err)
		}
		c.NeedConfirm = nc != 0
		out = append(out, c)
	}
	return out, rows.Err()
}

// Update 改名称/命令/目录。整行覆盖语义（PUT 那种 patch 半截的先不做）。
func Update(db *store.DB, id int64, in Command) error {
	c, err := in.normalized()
	if err != nil {
		return err
	}
	res, err := db.SqlDB().Exec(
		`UPDATE quick_commands SET name=?,command=?,cwd=?,need_confirm=? WHERE id=?`,
		c.Name, c.Command, c.Cwd, b2i(c.NeedConfirm), id)
	if err != nil {
		return fmt.Errorf("改快捷命令: %w", err)
	}
	return oneOrNotFound(res)
}

// Delete 删掉一条。
func Delete(db *store.DB, id int64) error {
	res, err := db.SqlDB().Exec(`DELETE FROM quick_commands WHERE id=?`, id)
	if err != nil {
		return fmt.Errorf("删快捷命令: %w", err)
	}
	return oneOrNotFound(res)
}

// Move 与相邻那一条交换位置（dir 只接受 "up"/"down"）。
//
// 为什么不是 sort+delta：sort 以 10 为间隔创建，"减一点"跨不过去 —— 20 变
// 19 之后与 10 的先后关系一模一样，用户按上移键看到的是"什么都没发生"，
// 而且一次错都不报（实测：sort 20→19，列表原地不动）。UI 上的上移/下移
// 就是"与旁边那条换位置"；要置顶就按到底。
//
// 做法是"读出顺序 → 与邻居换 sort"，而不是一句 UPDATE：列表本来就只
// 有个位数到几十条，读一遍比写一条跨行相关的子查询好懂得多，而且与 List
// 共用同一个排序表达式，不会出现"看着是第二条、按下上移动的却是另一条"。
//
// 边界处（已在最前/最后）不动也不报错：连点到底之后再按是手熟，报 400
// 只会多一个前端要吞掉的错误分支。
func Move(db *store.DB, id int64, dir string) error {
	switch dir {
	case "up", "down":
	default:
		return fmt.Errorf("非法排序方向 %q", dir)
	}
	items, err := List(db)
	if err != nil {
		return err
	}
	at := -1
	for i, it := range items {
		if it.ID == id {
			at = i
			break
		}
	}
	if at < 0 {
		return ErrNotFound
	}
	other := at - 1
	if dir == "down" {
		other = at + 1
	}
	if other < 0 || other >= len(items) {
		return nil // 已经在头/尾
	}
	// 换位置后整表重排 sort（10、20、30…）。
	//
	// 比"只把两条的 sort 对调"更严：对调在两条 sort 本来就相同时是空操作
	// —— 移动会静默失效。整表重排保证任意数据（含历史脏数据）按 List 给出
	// 的顺序变成严格递增，顺带把"同值时谁在前"这个问题从根上消掉。
	// 代价是 n 次 UPDATE，而 n 是个位数到几十，比一条跨行相关的子查询好懂。
	//
	// 必须在一个事务里：逐条裸写的话中间态会被并发请求读到（一堆相同 sort），
	// 用户看到的是列表顺序闪一下又跳回来。
	order := make([]Command, len(items))
	copy(order, items)
	order[at], order[other] = order[other], order[at]

	tx, err := db.SqlDB().Begin()
	if err != nil {
		return fmt.Errorf("开始排序事务: %w", err)
	}
	defer tx.Rollback()
	for i, it := range order {
		want := int64((i + 1) * 10)
		if it.Sort == int(want) {
			continue // 位置没变的不写：省掉绝大多数行的写放大
		}
		if _, err := tx.Exec(`UPDATE quick_commands SET sort=? WHERE id=?`, want, it.ID); err != nil {
			return fmt.Errorf("重排排序位: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("提交排序: %w", err)
	}
	return nil
}

// Get 取一条，供 run 接口按 id 找命令。
func Get(db *store.DB, id int64) (Command, error) {
	var (
		c  Command
		nc int
	)
	err := db.SqlDB().QueryRow(
		`SELECT id,name,command,cwd,need_confirm,sort,created_at
		 FROM quick_commands WHERE id=?`, id).
		Scan(&c.ID, &c.Name, &c.Command, &c.Cwd, &nc, &c.Sort, &c.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return Command{}, ErrNotFound
	}
	if err != nil {
		return Command{}, fmt.Errorf("读快捷命令 %d: %w", id, err)
	}
	c.NeedConfirm = nc != 0
	return c, nil
}

func oneOrNotFound(res interface{ RowsAffected() (int64, error) }) error {
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("确认影响行数: %w", err)
	}
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

func b2i(b bool) int {
	if b {
		return 1
	}
	return 0
}
