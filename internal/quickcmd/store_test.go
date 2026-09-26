package quickcmd

import (
	"errors"
	"strings"
	"testing"
)

// ---------- 校验 ----------

// 名称与命令内容都必填。校验放在存储层而不是 HTTP 层：API 那层的职责是
// 把错误映射成状态码，不是重新定义什么算合法输入（终端会话那边同样是这么
// 分的，两处各写一套校验迟早漂开）。
func TestCommandValidation(t *testing.T) {
	cases := []struct {
		why  string
		in   Command
		want error
	}{
		{"名字为空", Command{Name: "   ", Command: "ls"}, ErrNameRequired},
		{"命令为空", Command{Name: "看目录", Command: "  "}, ErrCommandRequired},
		{"两个都空", Command{}, ErrNameRequired},
	}
	for _, c := range cases {
		t.Run(c.why, func(t *testing.T) {
			_, err := c.in.normalized()
			if !errors.Is(err, c.want) {
				t.Fatalf("要 %v, got %v", c.want, err)
			}
		})
	}
}

// 空白应当被清掉：名字带首尾空格会让列表里出现"看起来一样"的两行，
// 而命令带首尾空格会原样送进 shell（前导空格在有些 shell 里不进 history）。
func TestCommandNormalization(t *testing.T) {
	got, err := Command{Name: "  重启  ", Command: "  systemctl restart nginx\n"}.normalized()
	if err != nil {
		t.Fatal(err)
	}
	if got.Name != "重启" {
		t.Fatalf("名字没清干净: %q", got.Name)
	}
	if got.Command != "systemctl restart nginx" {
		t.Fatalf("命令没清干净: %q", got.Command)
	}
}

// 危险命令默认要二次确认。自动置真是为了"用户忘了勾"——这个开关防的
// 正是手滑，让手滑的人自己去勾等于没有。允许显式取消（自己写的 rm -rf
// 缓存目录，确认过就是确认过）。
func TestDangerousCommandsAutoConfirm(t *testing.T) {
	dangerous := []string{
		"rm -rf /tmp/x",
		"sudo rm -rf ./cache",
		"mkfs.ext4 /dev/sda1",
		"dd if=/dev/zero of=/dev/sda",
		"shutdown -h now",
		"reboot",
		"systemctl stop nginx",
		":(){ :|:& };:",
	}
	for _, cmd := range dangerous {
		t.Run(cmd, func(t *testing.T) {
			got, err := Command{Name: "n", Command: cmd}.normalized()
			if err != nil {
				t.Fatal(err)
			}
			if !got.NeedConfirm {
				t.Fatalf("危险命令没自动置 need_confirm: %q", cmd)
			}
		})
	}

	// 安全这一侧只放“不命中任何危险子串”的命令。故意不放 `echo rm -rf`：
	// 那种样本在考“能不能识别出只是回显”，而要答对它就得去解析参数位置，
	// 而误判一次的代价是真执行了一条危险命令。这里保守宁可多弹一次确认。
	safe := []string{"ls -la", "df -h", "echo done", "git status", "du -sh .", "systemctl status nginx"}
	for _, cmd := range safe {
		t.Run("安全:"+cmd, func(t *testing.T) {
			got, err := Command{Name: "n", Command: cmd}.normalized()
			if err != nil {
				t.Fatal(err)
			}
			if got.NeedConfirm {
				t.Fatalf("普通命令不该弹确认: %q", cmd)
			}
		})
	}

	// 这个开关不可取消：危险命令一律 need_confirm=true，传 false 也会被抬回去。
	// 它防的就是手滑与“当时没多想”，能取消等于没有；而且命令内容是用户
	// 自己填的，“我写的 rm -rf 肯定是安全的”这类判断不应由面板代答。
	got, err := Command{Name: "n", Command: "rm -rf ./cache", NeedConfirm: false}.normalized()
	if err != nil {
		t.Fatal(err)
	}
	if !got.NeedConfirm {
		t.Fatal("危险命令的确认不许被关掉")
	}
}

// ---------- 存储 ----------

func TestCommandStoreCRUD(t *testing.T) {
	db := openDB(t)

	created, err := Create(db, Command{Name: "看磁盘", Command: "df -h", Cwd: "/var/log"})
	if err != nil {
		t.Fatal(err)
	}
	if created.ID <= 0 {
		t.Fatalf("没拿到 id: %+v", created)
	}
	if created.CreatedAt <= 0 {
		t.Fatalf("没记 created_at: %+v", created)
	}

	items, err := List(db)
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 || items[0].Command != "df -h" || items[0].Cwd != "/var/log" {
		t.Fatalf("读回不对: %+v", items)
	}

	if err := Update(db, created.ID, Command{Name: "磁盘使用", Command: "du -sh .", Cwd: ""}); err != nil {
		t.Fatal(err)
	}
	items, _ = List(db)
	if items[0].Name != "磁盘使用" || items[0].Command != "du -sh ." {
		t.Fatalf("改名/改命令没生效: %+v", items[0])
	}

	if err := Delete(db, created.ID); err != nil {
		t.Fatal(err)
	}
	items, _ = List(db)
	if len(items) != 0 {
		t.Fatalf("删除后还剩 %d 条", len(items))
	}
}

// 表必须由迁移建出来：装配层写的 SQL 引用了没迁移的表，只会在第一次
// 真的有人点快捷命令时炸，而那时已经部署完了。
func TestQuickCommandsTableExists(t *testing.T) {
	db := openDB(t)
	if _, err := db.SqlDB().Exec(
		`INSERT INTO quick_commands(name,command,cwd,need_confirm,sort,created_at)
		 VALUES('n','c','',0,0,1)`); err != nil {
		t.Fatalf("quick_commands 表不可用: %v", err)
	}
}

// 顺序是用户摆的，必须稳定：靠 id 隐式排序的话，删掉中间一条再补一条
// 就会把顺序打乱，而"常用的那条"排在最后是很烦的事。
func TestCommandSortOrder(t *testing.T) {
	db := openDB(t)
	for _, n := range []string{"一", "二", "三"} {
		if _, err := Create(db, Command{Name: n, Command: "true"}); err != nil {
			t.Fatal(err)
		}
	}
	items, _ := List(db)
	var names []string
	for _, it := range items {
		names = append(names, it.Name)
	}
	if strings.Join(names, ",") != "一,二,三" {
		t.Fatalf("创建顺序没保持: %v", names)
	}

	// 上移两次到最前。
	//
	// 这里原先写的是 Move(id, -100) —— 一次挪一大步。那个契约本身就是错的
	// （见 TestMoveSwapsWithNeighbor），因为按 delta 实现 ±1 时跨不过 10
	// 的间隔。UI 上的上移键本来就是"与相邻那条换位置"，要置顶就按到底。
	for i := 0; i < 2; i++ {
		if err := Move(db, items[2].ID, "up"); err != nil {
			t.Fatal(err)
		}
	}
	items, _ = List(db)
	if items[0].Name != "三" {
		t.Fatalf("上移两次没到最前: %s", items[0].Name)
	}
}

// 上移/下移必须真的越过**相邻那一条**，而不是只把 sort 挪一点。
//
// sort 以 10 为间隔创建（为的是将来能往中间插），于是 sort±1 这类实现
// 跨不过去：10 与 20 之间做 20-1=19，列表顺序一模一样。用户看到的是
// "点上移，什么都不发生"，而且每一下都点得动、一次错都不报。
// （实测输出：sort 从 20 变 19，顺序原地不动。）
func TestMoveSwapsWithNeighbor(t *testing.T) {
	db := openDB(t)
	var ids []int64
	for _, n := range []string{"第一", "第二", "第三"} {
		c, err := Create(db, Command{Name: n, Command: "x"})
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, c.ID)
	}
	want := func(wantNames ...string) {
		t.Helper()
		items, err := List(db)
		if err != nil {
			t.Fatal(err)
		}
		var got []string
		for _, it := range items {
			got = append(got, it.Name)
		}
		if strings.Join(got, ",") != strings.Join(wantNames, ",") {
			t.Fatalf("顺序应为 %v, got %v", wantNames, got)
		}
	}
	want("第一", "第二", "第三")

	if err := Move(db, ids[1], "up"); err != nil {
		t.Fatal(err)
	}
	want("第二", "第一", "第三")

	// down 用**另一条**来按，而不是把刚才那条移回去：后者在"up/down 走
	// 了同一个分支"的实现上也会通过。
	// 现状 [第二 第一 第三]，把"第一"下移 → 应与它下面的"第三"换。
	if err := Move(db, ids[0], "down"); err != nil {
		t.Fatal(err)
	}
	want("第二", "第三", "第一")

	// 连续上移：每一步都要有效（"只换一次"的实现会在第二步停下）
	if err := Move(db, ids[2], "up"); err != nil {
		t.Fatal(err)
	}
	want("第三", "第二", "第一")
	if err := Move(db, ids[1], "up"); err != nil {
		t.Fatal(err)
	}
	want("第二", "第三", "第一")

	// 已经在最前的那条再上移：顺序不变、不报错，更不能把它甩到末尾
	if err := Move(db, ids[1], "up"); err != nil {
		t.Fatalf("边界处再上移不该报错: %v", err)
	}
	want("第二", "第三", "第一")
	// 已经在最后的那条再下移，同理
	if err := Move(db, ids[0], "down"); err != nil {
		t.Fatalf("边界处再下移不该报错: %v", err)
	}
	want("第二", "第三", "第一")
}

// 不存在的 id 上移必须报错：一条都改不动时静默成功会让前端以为挪好了。
func TestMoveUnknownID(t *testing.T) {
	db := openDB(t)
	if err := Move(db, 9999, "up"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("应 ErrNotFound, got %v", err)
	}
}

// 非法方向必须报错，而不是被当成"往下"。
func TestMoveRejectsBadDir(t *testing.T) {
	db := openDB(t)
	c, err := Create(db, Command{Name: "x", Command: "x"})
	if err != nil {
		t.Fatal(err)
	}
	if err := Move(db, c.ID, "sideways"); err == nil {
		t.Fatal("非法方向该报错")
	}
}
