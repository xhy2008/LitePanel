import { describe, it, expect } from 'vitest';
import { affectedDirs, touchesDir, justFinished } from '../components/files/jobDirs';

// 任务完成后"该不该刷新当前目录"的判断。判断错的两种后果都不报错：
// 漏刷 = 删完列表不动；多刷 = 用户在翻长列表时被弹回顶部。

function job(op: 'copy' | 'move' | 'delete', src: string[], dst = '') {
  return { op, src, dst };
}

describe('affectedDirs', () => {
  it('删除：只认源文件的父目录', () => {
    expect(affectedDirs(job('delete', ['/data/a.txt', '/data/b.txt']))).toEqual(['/data']);
  });

  // 移动会把源那一侧也清空：只认 dst 会让"被剪切走的那个目录"停在旧列表上。
  it('移动：目标目录 + 每个源的父目录都算', () => {
    const dirs = affectedDirs(job('move', ['/a/x.txt'], '/b'));
    expect(dirs).toContain('/b');
    expect(dirs).toContain('/a');
  });

  it('去重：同目录多个文件只算一次', () => {
    expect(affectedDirs(job('copy', ['/d/1', '/d/2'], '/d'))).toEqual(['/d']);
  });

  it('根的父还是根（parentOf("/")="/"，不会算出空串）', () => {
    expect(affectedDirs(job('delete', ['/x.txt']))).toEqual(['/']);
  });
});

describe('touchesDir', () => {
  it('当前目录在受影响集合里才算数', () => {
    expect(touchesDir(job('delete', ['/data/a']), '/data')).toBe(true);
    expect(touchesDir(job('delete', ['/data/a']), '/other')).toBe(false);
  });

  // 空目录 = 还没加载出任何目录，没有"正在看的东西"可刷。
  it('dir 为空一律 false', () => {
    expect(touchesDir(job('delete', ['/data/a']), '')).toBe(false);
  });
});

describe('justFinished', () => {
  // 两个条件（见 jobDirs.ts 注释）：上一帧活跃、现在终态。
  it('running → done 才算刚跑完', () => {
    expect(justFinished(true, { state: 'done' } as never)).toBe(true);
  });

  // failed 也要刷：copy 在后端**不回滚**（exec_run.go），传到一半失败会把
  // 那一半留在目标目录，不刷就是漏刷。delete/move 虽是整批成败（failed 时
  // 磁盘没变），但多刷一次无害。两种并成一律刷，宁多勿漏。
  it('failed 也算跑完了（copy 不回滚，可能留下半个目标）', () => {
    expect(justFinished(true, { state: 'failed' } as never)).toBe(true);
  });

  // canceled 能停在半途，已搬走的文件真在盘上 —— 与 failed 相反,要刷。
  it('canceled 要刷（可能已搬走一部分）', () => {
    expect(justFinished(true, { state: 'canceled' } as never)).toBe(true);
  });

  // 冷启动 load 出一批历史任务时，它们上一帧不存在，不能集体触发刷新。
  it('上一帧不存在（undefined）不算刚跑完', () => {
    expect(justFinished(undefined, { state: 'done' } as never)).toBe(false);
  });

  // 重复的 done→done 推送（每 200ms 落库会连推几帧）不该反复触发。
  it('还在跑的时候不算刚跑完', () => {
    expect(justFinished(true, { state: 'running' } as never)).toBe(false);
    expect(justFinished(true, { state: 'pending' } as never)).toBe(false);
  });
});
