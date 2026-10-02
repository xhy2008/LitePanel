package filemgr

// RetryJob：把一条 interrupted 任务放回队列（设计 712 行"可一键重试"）。
//
// 这个函数本身只做状态检查与清零，看起来是整件事里最简单的部分。真正的
// 难点全在"重跑时怎么对待上次跑了一半留下的盘上状态"，而那部分**不在这里**,
// 在各执行体里（deleteMany / movePath / copyInto）—— 因为只有执行体知道
// 每个 op 的"完成态长什么样"。把判定放在这里会变成：重试逻辑知道"move 的
// 源可能已经没了"，却不知道没了之后该补哪一步。
//
// 状态清零（进度 + 取消意图）是必须的：不清进度的话界面上会出现"已完成
// 8/10 → 重试 → 已完成 8/10"，而执行体其实是从头跑的，用户看到"只做了
// 两个"。清 cancel_requested 同理 —— 上一轮点过的取消不该跟着这条任务
// 转世，否则重试被自己的历史拒掉。
//
// 只接受 interrupted，其他状态各有各的不可重试理由（见测试）。

import (
	"context"
	"fmt"
)

// RetryJob 把一条 interrupted 任务重置成 pending 并唤醒 worker。
func (s *Service) RetryJob(ctx context.Context, id int64) (Job, error) {
	db, err := s.jobDB()
	if err != nil {
		return Job{}, err
	}
	now := s.clock().Unix()
	// 状态、进度、取消意图三件事在**同一条语句**里改：分开写的话中间崩一次,
	// 可能留下"pending 但进度还是上次的 8/10"，界面上就是一条永远差两个的
	// 进度条。WHERE 里带上 state 是并发的正确姿势 —— 读-判-改是两步，
	// 期间任务可能被人抢先跑掉了。
	res, err := db.ExecContext(ctx,
		`UPDATE fs_jobs
		 SET state='pending', done_bytes=0, entries_done=0,
		     cancel_requested=0, error=NULL, resumed=1, updated_at=?
		 WHERE id=? AND state='interrupted'`, now, id)
	if err != nil {
		return Job{}, fmt.Errorf("重试任务: %w", err)
	}
	if c, _ := res.RowsAffected(); c != 1 {
		// 没改到行：要么没这个 id，要么状态不是 interrupted。两者要能区分
		// ——前端对"不存在"该把这行删掉，对"状态不对"该刷新成现在的样子。
		j, err := s.GetJob(ctx, id)
		if err != nil {
			return Job{}, err
		}
		return Job{}, fmt.Errorf("%w: 当前状态 %s（只有被中断的任务能重试）",
			ErrJobNotCancellable, j.State)
	}
	s.kick()
	return s.GetJob(ctx, id)
}
