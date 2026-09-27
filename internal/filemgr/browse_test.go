package filemgr

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// 目录列举（设计 8.1）。测试的地基是两条真实性：
//   - 排序/分页的正确性用"混合类别"的夹具验证（目录+文件+中文名+空格名），
//     不是只放三个 a.txt b.txt；
//   - 错误路径断言的是"可读的错误"，不是"有错误"。

func mkDir(t *testing.T, names ...string) string {
	t.Helper()
	dir := t.TempDir()
	for _, n := range names {
		full := filepath.Join(dir, n)
		if strings.HasSuffix(n, "/") {
			if err := os.MkdirAll(full, 0o755); err != nil {
				t.Fatal(err)
			}
			continue
		}
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(strings.Repeat("x", len(n))), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func namesOf(p ListPage) []string {
	out := make([]string, 0, len(p.Entries))
	for _, e := range p.Entries {
		out = append(out, e.Name)
	}
	return out
}

func TestListBasics(t *testing.T) {
	dir := mkDir(t, "b.txt", "a.md", "子目录/", "带 空格.txt", ".hidden")
	// 目录的 mtime 是"刚刚"—— TempDir 全是此刻造的，mtime 排序测不出稳定序。
	// 稳定序的断言放在名字排序里；这里测形状。
	p, err := List(context.Background(), dir, ListOptions{})
	if err != nil {
		t.Fatal(err)
	}
	// 默认隐藏不进 Entries；Total 是过滤后的可见数（分页器拿它算页数，
	// 更精确的断言在 TestListTotalCountsVisible）。
	if p.Total != 4 {
		t.Fatalf("Total 应为可见条目数: %d", p.Total)
	}
	if got := namesOf(p); strings.Contains(strings.Join(got, ","), ".hidden") {
		t.Fatalf("默认不显示隐藏文件: %v", got)
	}
	if p.Path == "" {
		t.Fatal("Path 必须回显（前端面包屑用它，不能只信自己发出去的参数）")
	}
	for _, e := range p.Entries {
		if e.Name == "子目录" && !e.IsDir {
			t.Fatal("子目录必须 is_dir")
		}
		if e.Name == "a.md" && e.MIME != "text/markdown" {
			t.Fatalf("mime 粗判不对: %+v", e)
		}
		if e.Name == "子目录" && e.MIME != "inode/directory" {
			t.Fatalf("目录 mime: %+v", e)
		}
		if e.Name == "a.md" && e.Size != 4 { // "x"*len("a.md")=4
			t.Fatalf("size 不对: %+v", e)
		}
	}
}

// Total 是**过滤后**的总数：分页器拿它算页数，如果隐藏文件没显示却计入
// Total，最后一页永远是空的，用户可以无限翻页翻到怀疑人生。
func TestListTotalCountsVisible(t *testing.T) {
	dir := mkDir(t, "a", "b", ".h1", ".h2")
	p, err := List(context.Background(), dir, ListOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if p.Total != 2 {
		t.Fatalf("隐藏关闭时 Total 只数可见条目, got %d", p.Total)
	}
	p2, err := List(context.Background(), dir, ListOptions{Hidden: true})
	if err != nil {
		t.Fatal(err)
	}
	if p2.Total != 4 || len(p2.Entries) != 4 {
		t.Fatalf("Hidden 打开应全见: total=%d n=%d", p2.Total, len(p2.Entries))
	}
}

func TestListDirsFirstThenName(t *testing.T) {
	dir := mkDir(t, "z-dir/", "a.txt", "B.txt", "m-dir/", "c.md")
	p, err := List(context.Background(), dir, ListOptions{Sort: SortName})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"m-dir", "z-dir", "a.txt", "B.txt", "c.md"}
	got := namesOf(p)
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("目录优先+名称升序: got %v want %v", got, want)
	}
	// 降序：类别优先不变，类内反转。目录永远在前是资源管理器的核心习惯，
	// 降序把它翻到后面会让"文件夹被文件淹没"，用户找不到目录。
	p2, err := List(context.Background(), dir, ListOptions{Sort: SortName, Desc: true})
	if err != nil {
		t.Fatal(err)
	}
	got2 := namesOf(p2)
	if got2[0] != "m-dir" && got2[0] != "z-dir" {
		t.Fatalf("降序时目录仍然必须在前: %v", got2)
	}
	if got2[0] != "z-dir" || got2[1] != "m-dir" {
		t.Fatalf("目录类内应降序: %v", got2)
	}
	if got2[len(got2)-1] != "a.txt" {
		t.Fatalf("文件类内应降序（a 垫底）: %v", got2)
	}
}

// size 排序要 stat。这是唯一需要全量 stat 的排序键 —— 分页省不掉它，
// 因为不 stat 就不知道谁大。实现必须在注释里承认这点（见 browse.go），
// 并在 10 万目录上用 ctx 取消兜底。
func TestListSortBySizeMtime(t *testing.T) {
	dir := mkDir(t, "小.txt", "中间一点的.txt", "目录/")
	// 制造可区分的 mtime：小 最新，中 Middleware，目录 最旧
	now := timeNowForTest()
	mustChtimes(t, filepath.Join(dir, "目录"), now.Add(-30_000_000_000)) // -30s
	mustChtimes(t, filepath.Join(dir, "中间一点的.txt"), now.Add(-20_000_000_000))
	mustChtimes(t, filepath.Join(dir, "小.txt"), now.Add(-10_000_000_000))

	p, err := List(context.Background(), dir, ListOptions{Sort: SortSize})
	if err != nil {
		t.Fatal(err)
	}
	got := namesOf(p)
	// 目录恒在前；文件按 size 升序。夹具写的是 len(name) 个字节：
	// "小.txt"=7（小 是 3 字节）< "中间一点的.txt"=19（5 个汉字 15 字节）。
	// 数字写死在这里，是为了让"按 UTF-8 字节数还是按字符数"这个歧义
	// 有个可证伪的落点 —— 按字符排的话两条的相对顺序恰好不变，但夹具
	// 换个名字就会翻，所以钉住字节数。
	if got[0] != "目录" || got[1] != "小.txt" || got[2] != "中间一点的.txt" {
		t.Fatalf("size 排序不符: %v", got)
	}

	p2, err := List(context.Background(), dir, ListOptions{Sort: SortMtime})
	if err != nil {
		t.Fatal(err)
	}
	got2 := namesOf(p2)
	if got2[0] != "目录" || got2[1] != "中间一点的.txt" || got2[2] != "小.txt" {
		t.Fatalf("mtime 升序（旧在前）不符: %v", got2)
	}
}

// 类型排序 = 扩展名排序（资源管理器"类型"列的语义），同类内再按名字。
func TestListSortByType(t *testing.T) {
	dir := mkDir(t, "报告.md", "代码.go", "无扩展名", "另一个.md", "a-dir/")
	p, err := List(context.Background(), dir, ListOptions{Sort: SortType})
	if err != nil {
		t.Fatal(err)
	}
	got := namesOf(p)
	if got[0] != "a-dir" {
		t.Fatalf("目录仍在最前: %v", got)
	}
	// 文件按扩展名字典序："无扩展名"(无扩展=空串) < .go < .md
	if got[1] != "无扩展名" || got[2] != "代码.go" {
		t.Fatalf("扩展名排序不符: %v", got)
	}
	if got[3] != "另一个.md" || got[4] != "报告.md" {
		t.Fatalf("同扩展名内应按名字排: %v", got)
	}
}

func TestListPagination(t *testing.T) {
	var names []string
	for i := 0; i < 10; i++ {
		names = append(names, fmt.Sprintf("f%02d", i))
	}
	dir := mkDir(t, names...)
	seen := map[string]bool{}
	for page := 1; page <= 4; page++ { // 4 页 > 实际的 ceil(10/3)=4 页
		p, err := List(context.Background(), dir, ListOptions{Page: page, Size: 3})
		if err != nil {
			t.Fatal(err)
		}
		if p.Total != 10 {
			t.Fatalf("每页的 Total 必须一致: %d", p.Total)
		}
		for _, n := range namesOf(p) {
			if seen[n] {
				t.Fatalf("第 %d 页出现了重复条目 %s（页间必须不重叠）", page, n)
			}
			seen[n] = true
		}
	}
	if len(seen) != 10 {
		t.Fatalf("4 页 ×3 应覆盖全部 10 条, got %d", len(seen))
	}
	// 越界页：空列表而不是报错，也不是把最后一页再给一遍。
	p, err := List(context.Background(), dir, ListOptions{Page: 99, Size: 3})
	if err != nil {
		t.Fatal(err)
	}
	if len(p.Entries) != 0 {
		t.Fatalf("越界页必须是空: %v", namesOf(p))
	}
	// page/size 非法值收敛到默认，不报错也不返回全量
	p, err = List(context.Background(), dir, ListOptions{Page: 0, Size: 0})
	if err != nil || p.Size != DefaultPageSize || p.Page != 1 {
		t.Fatalf("非法分页参数应收敛到默认: %+v err=%v", p, err)
	}
	// size 有上限：page_size=10^9 的请求不该触发 10^9 次 stat（排序键是
	// size 时全量 stat 已经付了，但 entries 的构造与序列化必须被夹住）。
	p, err = List(context.Background(), dir, ListOptions{Size: 1 << 20})
	if err != nil {
		t.Fatal(err)
	}
	if p.Size > MaxPageSize {
		t.Fatalf("size 必须夹到上限: %d > %d", p.Size, MaxPageSize)
	}
}

// 空目录给空数组而不是 null：前端 typescript 那边 `entries.map` 对 null
// 直接 TypeError，整个文件页白屏。Go 侧保证非 nil 比让每个前端调用方
// 写 `?? []` 可靠。
func TestListEmptyDir(t *testing.T) {
	dir := t.TempDir()
	p, err := List(context.Background(), dir, ListOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if p.Entries == nil {
		t.Fatal("Entries 必须非 nil（JSON 序列化成 [] 而不是 null）")
	}
	if len(p.Entries) != 0 || p.Total != 0 {
		t.Fatalf("空目录: %+v", p)
	}
}

// 路径是普通文件而不是目录：ErrNotDirectory（API 映射 400）。
// 让它以 fs.ErrInvalid 或裸 ENOTDIR 冒出去会被 API 层归进 500。
func TestListOnFile(t *testing.T) {
	dir := mkDir(t, "f.txt")
	_, err := List(context.Background(), filepath.Join(dir, "f.txt"), ListOptions{})
	if !errors.Is(err, ErrNotDirectory) {
		t.Fatalf("应报 ErrNotDirectory, got %v", err)
	}
}

// 目录不存在：fs.ErrNotExist（API 映射 404）。列表与点击之间目录被删掉
// 是日常时序，不是输入错误。
func TestListMissingDir(t *testing.T) {
	_, err := List(context.Background(), filepath.Join(t.TempDir(), "没这个目录"), ListOptions{})
	if !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("应报 fs.ErrNotExist, got %v", err)
	}
}

// 无权限目录：报可读错误而不是 500（验收条款）。这里钉两个点：
// errors.Is(fs.ErrPermission) 供 API 层映射状态码；错误文本里带路径，
// 供人看出是哪个目录读不了。root 下 chmod 拦不住 root，跳过。
func TestListPermissionDenied(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root 无视权限位")
	}
	dir := mkDir(t, "a")
	secret := filepath.Join(dir, "secret")
	if err := os.Mkdir(secret, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(secret, "x"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(secret, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(secret, 0o755) })
	_, err := List(context.Background(), secret, ListOptions{})
	if !errors.Is(err, fs.ErrPermission) {
		t.Fatalf("应报 fs.ErrPermission, got %v", err)
	}
	if !strings.Contains(err.Error(), "secret") {
		t.Fatalf("错误必须带路径: %v", err)
	}
}

// 单个条目 stat 失败（读取与 stat 之间文件被删，或某个文件权限特殊）
// 不能让整页失败：列一半的目录也比整页 500 有用。失败条目跳过，
// 其余照常。这是"部分成功"语义，必须显式测试而不是祈祷不发生。
func TestListSurvivesEntryRace(t *testing.T) {
	dir := mkDir(t, "keep1", "gone", "keep2")
	gone := filepath.Join(dir, "gone")
	// 用符号链接指向不存在目标模拟"stat 得到但内容没了"的稳定形态：
	// lstat 成功、真实 size 读不到（悬空链接）。
	if err := os.Remove(gone); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(dir, "不存在"), gone); err != nil {
		t.Skipf("本机不支持符号链接: %v", err)
	}
	p, err := List(context.Background(), dir, ListOptions{Sort: SortSize})
	if err != nil {
		t.Fatalf("单个坏条目不该拖垮整页: %v", err)
	}
	got := strings.Join(namesOf(p), ",")
	if !strings.Contains(got, "keep1") || !strings.Contains(got, "keep2") {
		t.Fatalf("好条目必须都在: %v", got)
	}
	// 悬空符号链接本身：仍然要出现在列表里（它确实是目录里的一个条目，
	// 用户要看得见它才能删它）。
	if !strings.Contains(got, "gone") {
		t.Fatalf("悬空符号链接也必须列出: %v", got)
	}
	// 它的 size 是**链接自身**的长度（目标路径字符串的字节数），
	// 不是 0，也不是"目标的大小"（目标不存在）。只有列条目用 Lstat
	// 才有这个结果；Stat 会跟随链接、在悬空时整个失败。
	for _, e := range p.Entries {
		if e.Name != "gone" {
			continue
		}
		if want := int64(len(filepath.Join(dir, "不存在"))); e.Size != want {
			t.Fatalf("悬空链接 size 应为链接自身长度 %d: %+v", want, e)
		}
	}
}

// ctx 取消：10 万目录的 size 排序要 stat 全量，用户等不及切走了 ——
// 循环必须看 ctx，否则旧请求的 stat 风暴和新的列表请求抢同一块磁盘。
func TestListHonorsContext(t *testing.T) {
	dir := mkDir(t, "a", "b", "c")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := List(ctx, dir, ListOptions{})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("取消后必须立刻收手: %v", err)
	}

	// 第二形态：取消 + 越界空页。statRows 在空页上循环体一次都不执行，
	// 唯一能拦住这个请求的是入口检查 —— 少了它，已取消的请求会把自己的
	// readdir（10 万目录 = 280ms 的磁盘元数据读取）做完并**报告成功**。
	// 取消语义的底线恰恰是"绝不伪装成功"：调用方拿着一份成功响应会用它
	// 覆盖更新的数据。
	_, err = List(ctx, dir, ListOptions{Page: 9999})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("取消 + 越界空页也必须报 Canceled: %v", err)
	}
}

// timeNowForTest：本项目所有"时间"相关的测试都用可控时钟（M5 的教训：
// 真等 3 天的测试等于没有测试）。这里只需要一个稳定的"现在"来造 mtime
// 相对量，不用真接业务时钟。
func timeNowForTest() time.Time { return time.Now().Truncate(time.Second) }

func mustChtimes(t *testing.T, path string, ts time.Time) {
	t.Helper()
	if err := os.Chtimes(path, ts, ts); err != nil {
		t.Fatal(err)
	}
}

// foldKey 的边界。折叠排序键有两处会悄悄错：ASCII 快路径漏掉大写、
// 非 ASCII 走错分支。错的表现是"README 排到 readme 后面"这种用户
// 一眼看出、但单元测试不铺这个名字就永远测不出来的问题。
func TestFoldKey(t *testing.T) {
	cases := []struct{ in, want string }{
		{"readme.md", "readme.md"}, // 全小写：原样返回（不分配）
		{"README.md", "readme.md"}, // 纯大写
		{"ReadMe.MD", "readme.md"}, // 混合
		{"中文Name", "中文name"},       // 含非 ASCII 时整串交给 ToLower
		{"带空格 Name", "带空格 name"},   // 中文 + 空格 + 大写
		{"a1_B-2", "a1_b-2"},       // 数字与符号夹大写
		{".HIDDEN", ".hidden"},
	}
	for _, c := range cases {
		if got := foldKey(c.in); got != c.want {
			t.Errorf("foldKey(%q) = %q, 期望 %q", c.in, got, c.want)
		}
	}
}

// 折叠后同名（README.md / readme.md）必须有确定的第二级次序，
// 否则 sort 不稳定 → 同一目录两次列举可能给出不同页内容。
func TestListTiesAreStable(t *testing.T) {
	dir := mkDir(t, "README.md", "readme.md", "Readme.md")
	first, err := List(context.Background(), dir, ListOptions{Sort: SortName})
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 5; i++ {
		again, err := List(context.Background(), dir, ListOptions{Sort: SortName})
		if err != nil {
			t.Fatal(err)
		}
		if strings.Join(namesOf(again), ",") != strings.Join(namesOf(first), ",") {
			t.Fatalf("同大小写名字的目录两次列举次序不同: %v vs %v", namesOf(again), namesOf(first))
		}
	}
}

// IsSymlink 是链接标记。只靠"悬空链接 size 看着不对"间接可见等于没有契约：
// 正常链接（目标存在）也必须被标出来，否则用户看不出这是链接还是本体 ——
// 双击"进入"一个指向别的分区的链接，会让他以为自己还在当前分区里。
func TestListMarksSymlinks(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "目标文件")
	if err := os.WriteFile(target, []byte("hello"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(dir, "链接到文件")); err != nil {
		t.Skipf("本机不支持符号链接: %v", err)
	}
	if err := os.Symlink(filepath.Join(dir, "不存在"), filepath.Join(dir, "悬空链接")); err != nil {
		t.Fatal(err)
	}
	// 指向目录的链接：IsDir 必须是 false（它是链接，不是目录），
	// IsSymlink 是 true。前端据此显示"链接"角标 + 目录图标。
	linkDir := filepath.Join(dir, "链接到目录")
	if err := os.Symlink(filepath.Join(dir, "真实目录"), linkDir); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(dir, "真实目录"), 0o755); err != nil {
		t.Fatal(err)
	}

	p, err := List(context.Background(), dir, ListOptions{Sort: SortName})
	if err != nil {
		t.Fatal(err)
	}
	byName := map[string]Entry{}
	for _, e := range p.Entries {
		byName[e.Name] = e
	}
	// 4 个链接/文件 + 被指向的真实目录本身 = 5 条
	if len(byName) != 5 {
		t.Fatalf("应列出 5 条: %v", namesOf(p))
	}
	// 真实目录自己是目录、不是链接 —— 把"指向目录的链接"和"目录本身"
	// 搞混是 Stat/Lstat 用错的典型后果
	if e := byName["真实目录"]; !e.IsDir || e.IsSymlink {
		t.Errorf("真实目录应是目录且非链接: %+v", e)
	}
	// 指向目录的链接：is_dir=false（它是链接）・is_symlink=true
	if e := byName["链接到目录"]; e.IsDir || !e.IsSymlink {
		t.Errorf("指向目录的链接应 is_dir=false / is_symlink=true: %+v", e)
	}
	for _, n := range []string{"链接到文件", "悬空链接", "链接到目录"} {
		if e, ok := byName[n]; !ok {
			t.Fatalf("缺少 %s", n)
		} else if !e.IsSymlink {
			t.Errorf("%s 必须标 is_symlink: %+v", n, e)
		}
	}
	if e := byName["目标文件"]; e.IsSymlink {
		t.Errorf("普通文件不该标 is_symlink: %+v", e)
	}
	if e := byName["链接到文件"]; e.IsDir {
		t.Errorf("链接不标 is_dir（前端按链接渲染）: %+v", e)
	}
	// 链接的 size 走 Lstat：报链接自身的长度，不是目标的。
	// 报目标 size 会让用户按 size 排序时把一个 20 字节的链接当成
	// 它指向的 20GB 文件。
	if e := byName["链接到文件"]; e.Size != int64(len(target)) {
		t.Errorf("链接 size 应为链接目标字符串长度 %d, got %d", len(target), e.Size)
	}
	if e := byName["目标文件"]; e.Size != 5 {
		t.Errorf("普通文件 size: %+v", e)
	}
}

// mode 也一样要走 Lstat：0777 lrwxrwxrwx 才是链接的真实权限位，
// 跟着目标显示 0644 会让人以为改权限改错了地方。
func TestListModeIsLinkOwn(t *testing.T) {
	dir := t.TempDir()
	f := filepath.Join(dir, "f")
	if err := os.WriteFile(f, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	l := filepath.Join(dir, "l")
	if err := os.Symlink(f, l); err != nil {
		t.Skipf("本机不支持符号链接: %v", err)
	}
	p, err := List(context.Background(), dir, ListOptions{Sort: SortName})
	if err != nil {
		t.Fatal(err)
	}
	var modeL, modeF string
	for _, e := range p.Entries {
		switch e.Name {
		case "l":
			modeL = e.Mode
		case "f":
			modeF = e.Mode
		}
	}
	if !strings.HasPrefix(modeL, "L") {
		t.Errorf("链接 mode 应以 L 开头（os.ModeSymlink）: %q", modeL)
	}
	if !strings.HasPrefix(modeF, "-") {
		t.Errorf("普通文件 mode 应以 - 开头: %q", modeF)
	}
}
