import { describe, it, expect, beforeEach, vi } from 'vitest';
import { setActivePinia, createPinia } from 'pinia';
import { setApi, resetApi } from '../api/inject';
import { useFsJobsStore } from '../stores/fsJobs';
import { jobPercent, isActive } from '../api/fsJobs';
import type { JobProgress, JobRow } from '../api/fsJobs';

function row(over: Partial<JobRow> = {}): JobRow {
  return {
    id: 1,
    op: 'delete',
    src: ['/data/a.txt'],
    dst: '',
    total_bytes: 0,
    done_bytes: 0,
    entries_total: 10,
    entries_done: 0,
    state: 'running',
    cancel_requested: false,
    permanent: false,
    resumed: false,
    created_at: 0,
    updated_at: 0,
    ...over,
  };
}

function prog(over: Partial<JobProgress> = {}): JobProgress {
  return {
    id: 1,
    op: 'delete',
    dst: '',
    total_bytes: 0,
    done_bytes: 0,
    entries_total: 10,
    entries_done: 5,
    state: 'running',
    cancel_requested: false,
    permanent: false,
    resumed: false,
    updated_at: 0,
    ...over,
  };
}

let calls: string[] = [];
function fakeApi(jobs: JobRow[] = []) {
  calls = [];
  return {
    get: vi.fn(async (path: string) => {
      calls.push('GET ' + path);
      return { jobs };
    }),
    post: vi.fn(async (path: string) => {
      calls.push('POST ' + path);
      if (path.endsWith('/retry')) return row({ id: 1, state: 'pending', resumed: true });
      // 后端受理响应同时带 id 与 job_id（jobResponse 嵌了 Job）。只回 id
      // 的替身会让 store 交出 undefined —— 那正是设计 709 行专门防的错
      // （前端拿 undefined 拼出 /fs/jobs/undefined）。这条替身必须两个都带。
      return { ...row({ id: 99, state: 'pending' }), job_id: 99 };
    }),
    del: vi.fn(async (path: string) => {
      calls.push('DELETE ' + path);
      return { canceled: true };
    }),
    patch: vi.fn(async () => ({})),
    put: vi.fn(async () => ({})),
  };
}

beforeEach(() => {
  setActivePinia(createPinia());
  resetApi();
});

describe('fsJobs store', () => {
  it('load 拉列表；后端回 null 时兜成空数组而不是崩', async () => {
    setApi(fakeApi([row()]) as any, { pathname: '/', search: '', assign: vi.fn() });
    const s = useFsJobsStore();
    await s.load();
    expect(s.items).toHaveLength(1);
    expect(s.loaded).toBe(true);

    setApi({ ...fakeApi(), get: vi.fn(async () => ({ jobs: null })) } as any, {
      pathname: '/',
      search: '',
      assign: vi.fn(),
    });
    const s2 = useFsJobsStore();
    await s2.load();
    expect(s2.items).toEqual([]);
  });

  it('activeCount 只数 pending 与 running', async () => {
    setApi(
      fakeApi([
        row({ id: 1, state: 'running' }),
        row({ id: 2, state: 'pending' }),
        row({ id: 3, state: 'done' }),
        row({ id: 4, state: 'failed' }),
        row({ id: 5, state: 'interrupted' }),
      ]) as any,
      { pathname: '/', search: '', assign: vi.fn() },
    );
    const s = useFsJobsStore();
    await s.load();
    expect(s.activeCount).toBe(2);
    expect(s.hasActive).toBe(true);
  });

  // applyProgress 的核心契约：合并而不覆盖 src。
  it('applyProgress 合并推送但保留 src（推送里没有它）', async () => {
    setApi(fakeApi([row({ src: ['/a', '/b', '/c'] })]) as any, {
      pathname: '/',
      search: '',
      assign: vi.fn(),
    });
    const s = useFsJobsStore();
    await s.load();
    s.applyProgress(prog({ entries_done: 8, state: 'done' }));
    expect(s.items[0].entries_done).toBe(8);
    expect(s.items[0].state).toBe('done');
    // 这条断言是全部要点：推送是整行的有界子集，视图仍要能读到 src。
    expect(s.items[0].src).toEqual(['/a', '/b', '/c']);
  });

  // 推一个列表里没有的 id：不能就地插入半截数据（那会缺 src），得重拉。
  it('收到未知 id 的推送时重拉而不是插进残缺行', async () => {
    setApi(fakeApi([]) as any, { pathname: '/', search: '', assign: vi.fn() });
    const s = useFsJobsStore();
    await s.load();
    s.applyProgress(prog({ id: 777 }));
    await Promise.resolve();
    expect(calls).toContain('GET /api/fs/jobs');
    expect(s.items.find((i) => i.id === 777)).toBeUndefined();
  });

  it('submit 走 /fs/jobs 并立刻把这条并进列表', async () => {
    setApi(fakeApi([]) as any, { pathname: '/', search: '', assign: vi.fn() });
    const s = useFsJobsStore();
    const id = await s.submit({ op: 'delete', paths: ['/x'] });
    expect(id).toBe(99);
    expect(calls).toContain('POST /api/fs/jobs');
    // 提交后抽屉必须立刻有这一条：空的抽屉会被读成"没生效"，用户会再点一次。
    expect(s.items.some((j) => j.id === 99)).toBe(true);
  });

  // 取消不写终态：这是与后端"两个主人"规则对齐的那一半。
  it('cancel 只置 cancel_requested，绝不自己写 state', async () => {
    setApi(fakeApi([row({ id: 5, state: 'running' })]) as any, {
      pathname: '/',
      search: '',
      assign: vi.fn(),
    });
    const s = useFsJobsStore();
    await s.load();
    await s.cancel(5);
    expect(calls).toContain('DELETE /api/fs/jobs/5');
    expect(s.items[0].cancel_requested).toBe(true);
    expect(s.items[0].state).toBe('running'); // 仍是 running，终态由后端写
  });

  it('cancel 失败时不回滚成已取消、把错误留给界面', async () => {
    const api = fakeApi([row({ id: 5, state: 'running' })]);
    api.del = vi.fn(async () => {
      throw { code: 'not_cancellable', message: '已终态', status: 409 };
    });
    setApi(api as any, { pathname: '/', search: '', assign: vi.fn() });
    const s = useFsJobsStore();
    await s.load();
    await expect(s.cancel(5)).rejects.toBeTruthy();
    expect(s.items[0].cancel_requested).toBe(false);
    expect(s.error).toBe('已终态');
  });

  it('retry 提交到 /retry 并把行改回 pending', async () => {
    setApi(fakeApi([row({ id: 1, state: 'interrupted' })]) as any, {
      pathname: '/',
      search: '',
      assign: vi.fn(),
    });
    const s = useFsJobsStore();
    await s.load();
    await s.retry(1);
    expect(calls).toContain('POST /api/fs/jobs/1/retry');
    expect(s.items[0].state).toBe('pending');
    expect(s.items[0].resumed).toBe(true);
  });
});

describe('jobPercent', () => {
  // total_bytes 后端恒为 0。拿它当分母会得 NaN，NaN 进 style.width 会让
  // 进度条整个消失 —— 看起来"任务没动"，其实是界面算不出来。
  it('字节全 0 时按条目数算，而不是除出 NaN', () => {
    expect(jobPercent(row({ entries_total: 4, entries_done: 1 }))).toBe(25);
  });

  it('两个分母都是 0 时回 null（未知），不是 0%', () => {
    expect(jobPercent(row({ entries_total: 0, total_bytes: 0 }))).toBeNull();
  });

  it('条目数优先于字节数', () => {
    expect(
      jobPercent(row({ entries_total: 2, entries_done: 1, total_bytes: 1000, done_bytes: 900 })),
    ).toBe(50);
  });

  it('进度被夹在 [0,100]：后端中途写超不会画出 120% 的条', () => {
    expect(jobPercent(row({ entries_total: 2, entries_done: 3 }))).toBe(100);
  });
});

describe('isActive', () => {
  it('只有 pending/running 算进行中；interrupted 不算', () => {
    expect(isActive('pending')).toBe(true);
    expect(isActive('running')).toBe(true);
    for (const st of ['done', 'failed', 'canceled', 'interrupted'] as const) {
      expect(isActive(st)).toBe(false);
    }
  });
});
