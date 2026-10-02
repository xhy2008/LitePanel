package filemgr

// RetryJob：把 interrupted 任务放回队列。
//
// 这一族测试钉的不是"能不能重新排队"（那是一句 UPDATE），而是**重跑之前
// 盘上已经发生了一半的那部分怎么算**。这是"一键重试"这个按钮的全部难点：
// 中断发生在 N 个条目的中间，前 k 个已经落地，剩下 N-k 个没动。把整批原样
// 再跑一遍，会在第 1 个就撞死 —— 实测两种最自然的做法都必然失败：
//
//	copy   重跑 → "目标已存在"（第一次的产物挡住了自己）
//	delete 重跑 → "file does not exist"（第一次已经把它送进回收站）
//
// 所以每个 op 要按"能不能**证明**这个条目已经做完"分别处理，而不是笼统地
// 跳过或笼统地重跑。判错方向的代价都不小：该跳的没跳 → 按钮永远报错；
// 不该跳的跳了 → 把"目标真有个同名文件"这种必须让用户知道冲突悄悄吞掉。

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// 同盘 move 中断后重跑：已经搬走的跳过，没搬的搬完。
//
// 同盘 move 每条是一次原子 rename，所以中断点上每个条目只有两种状态：
// 搬完了，或根本没动。"src 没了而 dst 在"因此**可证明**是已完成，跳过它
// 不是"猜测成功"而是有依据的判定。
func TestRetryMoveSameDiskSkipsCompleted(t *testing.T) {
	e := newJobEnv2(t)
	e.sameFS = true
	dst := filepath.Join(e.fast, "dst")
	if err := os.MkdirAll(dst, 0o755); err != nil {
		t.Fatal(err)
	}
	a := e.mkFileSync(t, "fast", "a.txt")
	b := e.mkFileSync(t, "fast", "b.txt")
	// 模拟中断在半途：a 已经搬过去了
	if _, err := e.svc.movePath(context.Background(), a, dst, nopProgress); err != nil {
		t.Fatal(err)
	}
	j := e.mustInterrupted(t, JobInput{Op: OpMove, Src: []string{a, b}, Dst: dst})
	e.retry(t, j.ID)
	e.runOnePending(t)
	got := e.mustGet(t, j.ID)
	if got.State != JobDone {
		t.Fatalf("重试该跑完, got %s (%s)", got.State, got.Error)
	}
	if _, err := os.Stat(filepath.Join(dst, "b.txt")); err != nil {
		t.Errorf("第二个没搬过去: %v", err)
	}
}

// 跨盘 move 中断在"副本已校验通过、源还没删"时，重试要**把删源补完**。
//
// 这是唯一一个"两边都还在"的状态，也是最危险的一个：源与目标各有一份。
// 三种朴素做法都错 ——
//   - 直接重跑：撞"目标已存在"，按钮永远点不动；
//   - 当成已完成跳过：盘上永远留着两份，而抽屉里写着"完成"；
//   - 直接删掉目标重拷：把已经花掉的拷贝成本丢掉，而且万一是别人放的文件
//     就删了别人的东西。
//
// 正确的做法是先**校验**：两边内容一致才说明"复制+校验"确实做完了、只差
// 删源，那就只补删源。校验不过就说明这个 dst 不是我们的产物（或已经坏了）,
// 必须报错而不是动任何一边。
func TestRetryMoveCrossDiskFinishesPendingDelete(t *testing.T) {
	e := newJobEnv2(t)
	e.sameFS = false
	a := filepath.Join(e.fast, "a.bin")
	writeFileN(t, a, 4096)
	want := hashFile(t, a) // 源没了之后就没法再比对，先记下
	// 手工造出"复制完成但源未删"的中断现场
	if _, err := e.svc.copyFile(context.Background(), a, filepath.Join(e.slow, "a.bin"), nopProgress); err != nil {
		t.Fatal(err)
	}
	j := e.mustInterrupted(t, JobInput{Op: OpMove, Src: []string{a}, Dst: e.slow})
	e.retry(t, j.ID)
	e.runOnePending(t)
	got := e.mustGet(t, j.ID)
	if got.State != JobDone {
		t.Fatalf("该补完, got %s (%s)", got.State, got.Error)
	}
	// 源必须没了（这才是"移动"）
	if _, err := os.Stat(a); !errors.Is(err, os.ErrNotExist) {
		t.Error("两边都还留着：把移动当成了复制")
	}
	// 留下的那份必须就是原来那份内容 —— 只验"文件存在"的话，
	// "删了源、留下一份坏副本"也算通过，而那是最坏的结果。
	if h := hashFile(t, filepath.Join(e.slow, "a.bin")); h != want {
		t.Errorf("补完留下的副本内容不对: %s != %s", h, want)
	}
}

// 上一种情况里，目标那份与源**不一致**时必须报错，且两边都不许动。
//
// 内容不一致就说明 dst 不是我们这次复制的产物（可能是用户自己放的同名文件）。
// 此时删源 = 用户的原件被吃掉；删目标 = 删了用户的东西。唯一诚实的答案是
// 停下来让人看。
func TestRetryMoveCrossDiskMismatchKeepsBothSides(t *testing.T) {
	e := newJobEnv2(t)
	e.sameFS = false
	a := filepath.Join(e.fast, "a.bin")
	writeFileN(t, a, 4096)
	// 目标有一份**不一样**的同名文件
	if err := os.WriteFile(filepath.Join(e.slow, "a.bin"), []byte("别人的文件"), 0o644); err != nil {
		t.Fatal(err)
	}
	j := e.mustInterrupted(t, JobInput{Op: OpMove, Src: []string{a}, Dst: e.slow})
	e.retry(t, j.ID)
	e.runOnePending(t)
	got := e.mustGet(t, j.ID)
	if got.State != JobFailed {
		t.Fatalf("该失败而不是猜一边, got %s", got.State)
	}
	if _, err := os.Stat(a); err != nil {
		t.Errorf("源不该被动: %v", err)
	}
	if b, err := os.ReadFile(filepath.Join(e.slow, "a.bin")); err != nil || string(b) != "别人的文件" {
		t.Errorf("目标那份不该被动: %q %v", b, err)
	}
}

// delete 中断后重跑：已经进了回收站的跳过，剩下的删完。
//
// "src 不在了"本身不足以判定已完成 —— 也可能是用户自己在别处删了它。
// 判据要硬：回收站里**有这条 origin 指向它的条目**才算。少了这一步，
// "文件神秘消失"会被重试逻辑盖章成"删除成功"。
func TestRetryDeleteSkipsTrashed(t *testing.T) {
	e := newJobEnv2(t)
	e.sameFS = true
	a := e.mkFileSync(t, "fast", "a.txt")
	b := e.mkFileSync(t, "fast", "b.txt")
	// 中断现场：a 已经进了回收站
	if _, err := e.svc.Delete(context.Background(), a); err != nil {
		t.Fatal(err)
	}
	j := e.mustInterrupted(t, JobInput{Op: OpDelete, Src: []string{a, b}})
	e.retry(t, j.ID)
	e.runOnePending(t)
	got := e.mustGet(t, j.ID)
	if got.State != JobDone {
		t.Fatalf("重试该跑完, got %s (%s)", got.State, got.Error)
	}
	if _, err := os.Stat(b); !errors.Is(err, os.ErrNotExist) {
		t.Error("第二个没删")
	}
}

// 源不存在、**且**回收站里也没有它：仍然要报错。
//
// 这条与上一条只差"回收站里有没有"这一点证据，却必须走向相反的结果：
// 没有证据就说"这个文件已经删好了"，等于把一次数据丢失写成任务成功。
func TestRetryDeleteMissingWithoutEvidenceFails(t *testing.T) {
	e := newJobEnv2(t)
	a := e.mkFileSync(t, "fast", "a.txt")
	os.Remove(a) // 自己消失的，回收站里没有
	j := e.mustInterrupted(t, JobInput{Op: OpDelete, Src: []string{a}})
	e.retry(t, j.ID)
	e.runOnePending(t)
	got := e.mustGet(t, j.ID)
	if got.State != JobFailed {
		t.Fatalf("没有证据时该失败, got %s", got.State)
	}
}

// copy 中断后重跑撞见同名目标：仍然报"目标已存在"，**不**当作已完成跳过。
//
// copy 是唯一没有可靠"已完成"证据的 op —— 目标存在可能是我们上次拷的，
// 也可能是别人本来就放在那儿的同名文件，两者在盘上长得一模一样。跳过的
// 后果是把一次真实存在的命名冲突悄悄吞掉（用户以为拷成功了，而他的文件
// 还在那儿），这比"重试按下去又报一次同样的错"严重得多。
//
// 所以这里的"重试失败"是有意的行为，且与全新一次复制的语义完全一致。
func TestRetryCopyCollisionStillFails(t *testing.T) {
	e := newJobEnv2(t)
	e.sameFS = false
	a := filepath.Join(e.fast, "a.bin")
	writeFileN(t, a, 1024)
	if err := os.WriteFile(filepath.Join(e.slow, "a.bin"), []byte("别人放的"), 0o644); err != nil {
		t.Fatal(err)
	}
	j := e.mustInterrupted(t, JobInput{Op: OpCopy, Src: []string{a}, Dst: e.slow})
	e.retry(t, j.ID)
	e.runOnePending(t)
	got := e.mustGet(t, j.ID)
	if got.State != JobFailed {
		t.Fatalf("冲突该继续报错而不是跳过, got %s", got.State)
	}
	if b, err := os.ReadFile(filepath.Join(e.slow, "a.bin")); err != nil || string(b) != "别人放的" {
		t.Errorf("重试不许覆盖已存在的目标: %q %v", b, err)
	}
}

// RetryJob 只接受 interrupted。
//
// 其他终态各有各的不可重试理由，而把它们混成"都能重试"会有具体的破坏：
//   - 已 done 再跑一次：copy 会撞自己的产物，move 变成"源已不存在"，delete
//     变成"找不到文件"—— 全是假失败，用户会以为面板坏了；
//   - 已 canceled 再跑：用户明确说过别做，重启队列把它做掉，这跟
//     cancel_requested 不落库是同一类错误；
//   - 还在 pending/running：会造出同一批文件的第二个执行者（两个 worker
//     同时删一批路径，第二个必然 404，而用户看到的是"一次删除两条记录"）。
func TestRetryJobOnlyInterrupted(t *testing.T) {
	e := newJobEnv2(t)
	p := e.mkFileSync(t, "fast", "x.txt")
	for _, tc := range []struct {
		name  string
		state JobState
		ok    bool
	}{
		{"interrupted 可以", JobInterrupted, true},
		{"pending 不行", JobPending, false},
		{"running 不行", JobRunning, false},
		{"done 不行", JobDone, false},
		{"failed 不行", JobFailed, false},
		{"canceled 不行", JobCanceled, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := e.db.SqlDB().Exec(`INSERT INTO fs_jobs(op,src,entries_total,state,created_at,updated_at)
				VALUES('delete','["`+p+`"]',1,?,1,1)`, string(tc.state)); err != nil {
				t.Fatal(err)
			}
			var id int64
			if err := e.db.SqlDB().QueryRow(`SELECT last_insert_rowid()`).Scan(&id); err != nil {
				t.Fatal(err)
			}
			_, err := e.svc.RetryJob(context.Background(), id)
			if tc.ok && err != nil {
				t.Errorf("该能重试: %v", err)
			}
			if !tc.ok && err == nil {
				t.Errorf("%s 不该能重试", tc.state)
			}
		})
	}
}

// 重试会把上一次的进度清零。
//
// 不清零的话界面上会出现"已完成 8/10 → 重试 → 已完成 8/10"这种假象：
// 用户以为重试又从 8 开始做，而实际执行体是从头开始的（进度会从 8 再往上
// 走到 10，看起来"只做了 2 个"）。
func TestRetryResetsProgress(t *testing.T) {
	e := newJobEnv2(t)
	// done_bytes 也要给非零值：只给 entries_done 的话，"没清 done_bytes"
	// 这种实现漏清永远测不到（实测把 SET done_bytes=0 改成 done_bytes=
	// done_bytes 后原夹具照绿 —— 因为默认值本来就是 0）。两个进度维度都
	// 得从非零起，清零这条断言才真的钉住两个字段。
	if _, err := e.db.SqlDB().Exec(`INSERT INTO fs_jobs(op,src,entries_total,entries_done,done_bytes,state,created_at,updated_at)
		VALUES('delete','["/x"]',10,8,4096,'interrupted',1,1)`); err != nil {
		t.Fatal(err)
	}
	var id int64
	if err := e.db.SqlDB().QueryRow(`SELECT last_insert_rowid()`).Scan(&id); err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.RetryJob(context.Background(), id); err != nil {
		t.Fatal(err)
	}
	got, err := e.svc.GetJob(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	if got.EntriesDone != 0 || got.DoneBytes != 0 {
		t.Errorf("重试该清零进度, got entries=%d bytes=%d", got.EntriesDone, got.DoneBytes)
	}
	if got.State != JobPending {
		t.Errorf("重试该回到 pending, got %s", got.State)
	}
}

// 全新一次跨盘移动，目标已有一份**内容相同**的文件：仍然 409，不许合并。
//
// 这条是 resumed 那一列存在的全部理由。如果"内容一样就当是重试补删源"
// 对全新提交也成立，那么用户把一个文件移到已经放了一份同名同内容副本的
// 目录里时，面板会悄悄删掉他的原件（"反正两边一样")—— 而他从没要求移动
// 到那儿，更没要求删掉源。全新提交里没有"这是我们上次留下的副本"这个
// 前提，所以哪怕内容一致也只能报冲突。
//
// 判据靠 resumed 这个持久标志区分，而不是靠内容比对：内容比对分不开
// "我们拷的"与"用户本来就放在那儿的"。
func TestFreshMoveIdenticalTargetStillCollides(t *testing.T) {
	e := newJobEnv2(t)
	e.sameFS = false
	a := filepath.Join(e.fast, "a.bin")
	writeFileN(t, a, 2048)
	want := hashFile(t, a)
	// 目标放一份内容完全相同的文件（模拟用户自己的副本）
	if err := os.WriteFile(filepath.Join(e.slow, "a.bin"), mustRead(t, a), 0o644); err != nil {
		t.Fatal(err)
	}
	// 直接走非 resumed 路径（MoveMany = resumed:false）
	if _, err := e.svc.MoveMany(context.Background(), []string{a}, e.slow, nopProgress); !errors.Is(err, ErrExists) {
		t.Fatalf("全新移动撞同名（哪怕内容一样）该 ErrExists, got %v", err)
	}
	// 两边都必须还在
	if _, err := os.Stat(a); err != nil {
		t.Errorf("源不该被动: %v", err)
	}
	if hashFile(t, filepath.Join(e.slow, "a.bin")) != want {
		t.Error("目标那份不该被动")
	}
}

// 全新提交的任务（resumed=0）经由分派跑到"目标已存在且内容一致"时也
// 必须失败。
//
// 这条与 TestFreshMoveIdenticalTargetStillCollides 的分工是有意的：
// 那条直接调 MoveMany(resumed:false)，测的是内核；这一条走
// CreateJob → claim → runJob → runMove 的完整链路，测的是
// "runMove 有没有把 j.Resumed 原样传下去"。把 runMove 里的
// j.Resumed 写死成 true，内核测试照绿而线上每条新提交的任务都会
// 悄悄吃掉用户源文件 —— 只有这条链路测试会红。
func TestFreshMoveJobIdenticalTargetFails(t *testing.T) {
	e := newJobEnv2(t)
	e.sameFS = false
	a := filepath.Join(e.fast, "a.bin")
	writeFileN(t, a, 2048)
	if err := os.WriteFile(filepath.Join(e.slow, "a.bin"), mustRead(t, a), 0o644); err != nil {
		t.Fatal(err)
	}
	e.svc.StartJobs(mustPoolCtx(t))
	j, err := e.svc.CreateJob(context.Background(), JobInput{Op: OpMove, Src: []string{a}, Dst: e.slow})
	if err != nil {
		t.Fatal(err)
	}
	got := waitState(t, e.svc, j.ID, JobFailed, 5*time.Second)
	if !strings.Contains(got.Error, "已存在") {
		t.Errorf("该报目标已存在, got %q", got.Error)
	}
	// 源必须还在：不能被当成"重试补删源"吃掉
	if _, err := os.Stat(a); err != nil {
		t.Errorf("全新提交不该删源: %v", err)
	}
}
