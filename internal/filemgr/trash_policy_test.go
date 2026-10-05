package filemgr

// M7-T5：回收站两项设置的热生效。
//
// 复用 trash_test.go 的 trashEnv 夹具（三个假盘 + FilesystemRoot/TrashRoots
// 注入），而不是另造一个单盘夹具：Delete 要靠 FilesystemRoot 判"文件属于
// 哪个盘"，自造夹具要么漏掉注入（于是 Delete 直接失败），要么重新实现一遍
// 最长前缀匹配（两份实现必然漂移）。assemble() 还能模拟"面板重启"，
// 那正是验证"改名后条目是否真在盘上、而不是靠进程内缓存看得见"的手段。

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// 把某个盘回收站里所有条目的删除时间改成 at。
// 用 trashMeta 结构读写而不是拼 JSON 字面量：盘上格式是内部细节，写死在
// 测试里会让"改字段名"变成一次测试大面积变红，而那不是被测性质。
func rewriteTrashDeletedAt(t *testing.T, dir string, at time.Time) {
	t.Helper()
	des, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("读回收站 %s: %v", dir, err)
	}
	n := 0
	for _, de := range des {
		name := de.Name()
		if len(name) <= len(metaSuffix) || name[len(name)-len(metaSuffix):] != metaSuffix {
			continue
		}
		p := filepath.Join(dir, name)
		raw, err := os.ReadFile(p)
		if err != nil {
			t.Fatal(err)
		}
		var m trashMeta
		if err := json.Unmarshal(raw, &m); err != nil {
			t.Fatalf("meta 读不回来 %s: %v", p, err)
		}
		m.DeletedAt = at.Unix()
		out, err := json.Marshal(m)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, out, 0o600); err != nil {
			t.Fatal(err)
		}
		n++
	}
	if n == 0 {
		t.Fatalf("%s 下没有条目 meta", dir)
	}
}

// 换新名字的 Service（模拟"设置改了、面板重启"，没有任何进程内状态传过去）。
func (e *trashEnv) withDirName(name string) *Service {
	svc := e.assemble()
	svc.trashPolicy.Store(&trashPolicy{dirName: name, retain: svc.trash().retain})
	return svc
}

// 保留期只夹到边界，不接受 0（0 = 刚删就没，那是数据丢失而不是配置）。
func TestSetTrashPolicyClampsRetain(t *testing.T) {
	e := newTrashEnv(t)
	ctx := context.Background()
	if _, err := e.svc.SetTrashPolicy(ctx, ".trash", 0); err != nil {
		t.Fatal(err)
	}
	if got := e.svc.TrashRetain(); got != minTrashRetain {
		t.Errorf("0 应夹到下限，得 %v", got)
	}
	if _, err := e.svc.SetTrashPolicy(ctx, ".trash", 999*24*time.Hour); err != nil {
		t.Fatal(err)
	}
	if got := e.svc.TrashRetain(); got != maxTrashRetain {
		t.Errorf("越界应夹到上限，得 %v", got)
	}
}

// 目录名校验与 config/settings 同源：必须是单个路径段。
func TestSetTrashPolicyRejectsPathNames(t *testing.T) {
	e := newTrashEnv(t)
	for _, bad := range []string{"", " ", "/abs/.trash", "a/b", "..", "."} {
		if _, err := e.svc.SetTrashPolicy(context.Background(), bad, 3*24*time.Hour); err == nil {
			t.Errorf("目录名 %q 应被拒", bad)
		}
	}
	// 被拒时策略不能被改坏：半途改掉一半比报错更难排查。
	if e.svc.TrashDirName() != DefaultTrashDirName {
		t.Errorf("非法调用不应改动策略，现在是 %q", e.svc.TrashDirName())
	}
}

// 改名后**旧目录里的条目必须仍然看得见**。
//
// 这是这条设置真正危险的地方：列举与清理都按 <盘根>/<新名>/ 遍历，旧目录
// 里的条目会同时失去三个出口（界面看不到、清理不再碰、清空也扫不到）。
// 对一台管着用户文件的面板，那就是"删掉的文件不见了"，而且静默。
func TestRenameTrashKeepsOldEntriesVisible(t *testing.T) {
	e := newTrashEnv(t)
	ctx := context.Background()
	f := e.mk(t, "ssd", "keep.txt", "payload")
	if _, err := e.svc.Delete(ctx, f); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	d2 := e.mk(t, "disk", "other.txt", "x")
	if _, err := e.svc.Delete(ctx, d2); err != nil {
		t.Fatal(err)
	}
	before, err := e.svc.ListTrash(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(before) != 2 {
		t.Fatalf("改名前应能看到 2 条，得 %d", len(before))
	}

	if _, err := e.svc.SetTrashPolicy(ctx, ".lp_trash", 3*24*time.Hour); err != nil {
		t.Fatalf("改名失败: %v", err)
	}
	after, err := e.svc.ListTrash(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(after) != 2 {
		t.Fatalf("改名后条目消失了（剩 %d 条）—— 用户会以为文件丢了", len(after))
	}

	// 条目必须真的在**新目录**里（不是靠"仍在扫旧目录"糊过去的可见性）。
	entries, err := os.ReadDir(filepath.Join(e.ssd, ".lp_trash"))
	if err != nil {
		t.Fatalf("新回收站目录没建出来: %v", err)
	}
	var payload, meta int
	for _, en := range entries {
		if strings.HasSuffix(en.Name(), metaSuffix) {
			meta++
		} else {
			payload++
		}
	}
	if payload != 1 || meta != 1 {
		t.Errorf("新目录里载荷/meta 应为 1/1，得 %d/%d", payload, meta)
	}
	// 旧目录不该留下孤儿（整目录 rename 走）。
	if _, err := os.Lstat(filepath.Join(e.ssd, DefaultTrashDirName)); !os.IsNotExist(err) {
		t.Errorf("旧回收站目录还在，改名没搬干净")
	}

	// 换一个 Service 实例（模拟面板重启）后仍然看得见。
	if got, _ := e.withDirName(".lp_trash").ListTrash(ctx); len(got) != 2 {
		t.Errorf("重启后应仍有 2 条，得 %d", len(got))
	}
}

// 改名后还原仍要能成功（可见性之外，还原路径的推导也不能依赖旧目录名）。
func TestRenameTrashThenRestore(t *testing.T) {
	e := newTrashEnv(t)
	ctx := context.Background()
	f := e.mk(t, "ssd", "back.txt", "data")
	if _, err := e.svc.Delete(ctx, f); err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.SetTrashPolicy(ctx, ".lp_trash", 3*24*time.Hour); err != nil {
		t.Fatal(err)
	}
	items, err := e.svc.ListTrash(ctx)
	if err != nil || len(items) == 0 {
		t.Fatalf("列举失败: %v %d", err, len(items))
	}
	if _, err := e.svc.RestoreTrash(ctx, items[0].ID); err != nil {
		t.Fatalf("改名后还原失败: %v", err)
	}
	got, err := os.ReadFile(f)
	if err != nil || string(got) != "data" {
		t.Fatalf("还原后内容不对: %v %q", err, got)
	}
}

// 多个盘的旧条目都要搬（逐个盘遍历，不能只搬第一个）。
// 漏掉后面的盘的形态：系统盘上的删除看起来正常，机械盘上的条目全"消失"。
func TestRenameTrashCoversEveryDisk(t *testing.T) {
	e := newTrashEnv(t)
	ctx := context.Background()
	for _, which := range []string{"ssd", "disk", "home"} {
		f := e.mk(t, which, "f.txt", "x")
		if _, err := e.svc.Delete(ctx, f); err != nil {
			t.Fatalf("%s: %v", which, err)
		}
	}
	n, err := e.svc.SetTrashPolicy(ctx, ".lp_trash", 3*24*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if n != 3 {
		t.Errorf("三个盘各一条，应搬 3 条，得 %d", n)
	}
	for _, which := range []string{"ssd", "disk", "home"} {
		if _, err := os.Lstat(filepath.Join(e.root(which, ""), ".lp_trash")); err != nil {
			t.Errorf("%s 盘的新回收站目录不存在: %v", which, err)
		}
	}
	if got, _ := e.svc.ListTrash(ctx); len(got) != 3 {
		t.Errorf("改名后应看到 3 条，得 %d", len(got))
	}
}

// 新旧目录都在（用户以前用过新名字）时逐条合并，两边条目都要看得见。
func TestRenameTrashMergesIntoExistingDir(t *testing.T) {
	e := newTrashEnv(t)
	ctx := context.Background()
	f := e.mk(t, "ssd", "old.txt", "a")
	if _, err := e.svc.Delete(ctx, f); err != nil {
		t.Fatal(err)
	}
	// 手工建出新名字的目录并塞一条"以前删的"进去（模拟用户之前用过这个名字）。
	newDir := filepath.Join(e.ssd, ".lp_trash")
	if err := os.MkdirAll(newDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(newDir, "prev-payload"), []byte("p"), 0o600); err != nil {
		t.Fatal(err)
	}
	meta, _ := json.Marshal(trashMeta{Name: "prev.txt", Origin: filepath.Join(e.ssd, "prev.txt")})
	if err := os.WriteFile(filepath.Join(newDir, "prev-payload"+metaSuffix), meta, 0o600); err != nil {
		t.Fatal(err)
	}

	n, err := e.svc.SetTrashPolicy(ctx, ".lp_trash", 3*24*time.Hour)
	if err != nil {
		t.Fatalf("合并失败: %v", err)
	}
	if n != 1 {
		t.Errorf("应报告搬了 1 条，得 %d", n)
	}
	all, err := e.svc.ListTrash(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 2 {
		t.Fatalf("两边条目都该看得见，得 %d 条: %+v", len(all), all)
	}
}

// 没有 meta.json 的文件不是面板放的条目，改名时**不搬也不删**。
// 用户完全可能自己在那个目录里放了东西；按回收站规则处理它会误删。
func TestRenameTrashLeavesForeignFiles(t *testing.T) {
	e := newTrashEnv(t)
	ctx := context.Background()
	oldDir := filepath.Join(e.ssd, DefaultTrashDirName)
	if err := os.MkdirAll(oldDir, 0o700); err != nil {
		t.Fatal(err)
	}
	mine := filepath.Join(oldDir, "my-notes.txt")
	if err := os.WriteFile(mine, []byte("mine"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.SetTrashPolicy(ctx, ".lp_trash", 3*24*time.Hour); err != nil {
		t.Fatal(err)
	}
	// 整目录改名会把非条目文件一起带上新名字（它没被删，只是跟着目录走）。
	// 要钉的两条性质：文件还在（任一位置）、且没被当成回收站条目。
	if _, err := os.Lstat(mine); err != nil {
		if _, err2 := os.Lstat(filepath.Join(e.ssd, ".lp_trash", "my-notes.txt")); err2 != nil {
			t.Errorf("非条目的文件被弄丢了: %v / %v", err, err2)
		}
	}
	items, err := e.svc.ListTrash(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, it := range items {
		if it.Name == "my-notes.txt" {
			t.Errorf("非条目的文件被当成了回收站条目（会被还原/清空波及）: %+v", it)
		}
	}
}

// 保留期新值对**已存在**的条目立刻生效。
// 用户把它调到 1 天就是想快点清掉旧的；"只对新删除的生效"不是他期望的。
func TestRetainChangeAppliesToExistingEntries(t *testing.T) {
	e := newTrashEnv(t)
	ctx := context.Background()
	f := e.mk(t, "ssd", "gone.txt", "x")
	if _, err := e.svc.Delete(ctx, f); err != nil {
		t.Fatal(err)
	}
	// 必须用夹具的假时钟：Service 的到期判定走的是注入的 clock，
	// 拿真 time.Now() 改写会让条目落在假时钟的"未来"，永远不到期。
	rewriteTrashDeletedAt(t, filepath.Join(e.ssd, DefaultTrashDirName),
		e.clk.Now().Add(-10*24*time.Hour))

	if _, err := e.svc.SetTrashPolicy(ctx, DefaultTrashDirName, 30*24*time.Hour); err != nil {
		t.Fatal(err)
	}
	if n, err := e.svc.CleanTrash(ctx); err != nil || n != 0 {
		t.Fatalf("30 天保留期内不该清理，清了 %d 条 err=%v", n, err)
	}
	if _, err := e.svc.SetTrashPolicy(ctx, DefaultTrashDirName, 24*time.Hour); err != nil {
		t.Fatal(err)
	}
	if n, err := e.svc.CleanTrash(ctx); err != nil || n != 1 {
		t.Fatalf("改成 1 天后应立即清掉旧条目，清了 %d 条 err=%v", n, err)
	}
}

// 改名的同时改保留期：两项必须一起生效（一次设置提交两项）。
func TestSetTrashPolicyAppliesBothAtOnce(t *testing.T) {
	e := newTrashEnv(t)
	ctx := context.Background()
	f := e.mk(t, "ssd", "x.txt", "1")
	if _, err := e.svc.Delete(ctx, f); err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.SetTrashPolicy(ctx, ".lp_trash", 7*24*time.Hour); err != nil {
		t.Fatal(err)
	}
	if got := e.svc.TrashDirName(); got != ".lp_trash" {
		t.Errorf("目录名没生效: %q", got)
	}
	if got := e.svc.TrashRetain(); got != 7*24*time.Hour {
		t.Errorf("保留期没生效: %v", got)
	}
}

// 策略改动对在跑的读取方是原子的：不能出现"名字新的、保留期旧的"半更新。
// 本机 -race 不可用（Termux 跑不了），所以这条只能靠"读到的一定是某一次
// 完整写入"来断言，而不是指望竞态检测器帮忙发现。
func TestSetTrashPolicyIsAtomic(t *testing.T) {
	e := newTrashEnv(t)
	ctx := context.Background()
	type pair struct {
		name   string
		retain time.Duration
	}
	want := []pair{{".a", 2 * 24 * time.Hour}, {".b", 5 * 24 * time.Hour}, {".c", 9 * 24 * time.Hour}}
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < 3000; i++ {
			p := e.svc.trash()
			for _, w := range want {
				if p.dirName == w.name && p.retain != w.retain {
					t.Errorf("读到半更新的状态: %s / %v", p.dirName, p.retain)
					return
				}
			}
		}
	}()
	for _, w := range want {
		if _, err := e.svc.SetTrashPolicy(ctx, w.name, w.retain); err != nil {
			t.Fatal(err)
		}
	}
	<-done
}
