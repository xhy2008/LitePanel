import { describe, it, expect, vi, beforeEach } from 'vitest';
import { setActivePinia, createPinia } from 'pinia';
import { setApi, resetApi } from '../api/inject';
import { useDownloadsStore } from '../stores/downloads';
import type { DlTask } from '../api/downloads';

function task(over: Partial<DlTask> = {}): DlTask {
  return {
    gid: 'g1',
    uris: ['https://example.com/a.zip'],
    state: 'active',
    total_bytes: 100,
    done_bytes: 50,
    speed: 1024,
    connections: 3,
    created_at: 1,
    can_control: true,
    ...over,
  };
}

interface Opts {
  tasks?: DlTask[];
  tasksError?: unknown;
  putError?: unknown;
  postError?: unknown;
  delError?: unknown;
  health?: { ok: boolean; message?: string };
  healthError?: unknown;
}

function boot(o: Opts = {}) {
  setActivePinia(createPinia());
  const posts: string[] = [];
  const dels: string[] = [];
  let getTasks = 0;
  const api = {
    get: vi.fn(async (p: string) => {
      if (p === '/api/dl/tasks') {
        getTasks++;
        if (o.tasksError) throw o.tasksError;
        return { tasks: o.tasks ?? [] };
      }
      if (p === '/api/dl/summary') {
        return { speed: 0, active: 1, waiting: 0, paused: 0, done: 0, failed: 0 };
      }
      if (p === '/api/dl/health') {
        if (o.healthError) throw o.healthError;
        return o.health ?? { ok: true, version: '1.37.0', checked_at: 1 };
      }
      return { speed: 0, active: 1, waiting: 0, paused: 0, done: 0, failed: 0 };
    }),
    post: vi.fn(async (p: string) => {
      posts.push(p);
      if (o.postError) throw o.postError;
      return { ok: true };
    }),
    del: vi.fn(async (p: string) => {
      dels.push(p);
      if (o.delError) throw o.delError;
      return { ok: true, cleared: 2 };
    }),
    patch: vi.fn(async () => ({})),
    put: vi.fn(async () => ({})),
  };
  setApi(api as never, { pathname: '/', search: '', assign() {} } as never);
  return { api, posts, dels, taskFetches: () => getTasks };
}

// 合并窗口用真时间：假定时器与 store 内部的 setTimeout 交互容易测出
// 与真实行为不同的东西，300ms 的等待成本可以接受。
const tick = () => new Promise((r) => setTimeout(r, 350));

beforeEach(() => {
  resetApi();
});

describe('downloads store', () => {
  it('按 state 分四组；waiting 归入下载中', async () => {
    boot({
      tasks: [
        task({ gid: 'a', state: 'active' }),
        task({ gid: 'b', state: 'waiting' }),
        task({ gid: 'c', state: 'paused' }),
        task({ gid: 'd', state: 'complete' }),
        task({ gid: 'e', state: 'error' }),
      ],
    });
    const s = useDownloadsStore();
    await s.load();
    expect(s.active.map((t) => t.gid)).toEqual(['a', 'b']);
    expect(s.paused.map((t) => t.gid)).toEqual(['c']);
    expect(s.done.map((t) => t.gid)).toEqual(['d']);
    expect(s.failed.map((t) => t.gid)).toEqual(['e']);
  });

  it('health 失败不拖垮列表：历史任务照常展示', async () => {
    boot({
      tasks: [task({ state: 'complete' })],
      healthError: { message: 'connection refused' },
    });
    // health 单独 catch：aria2 掉了，历史任务（存在面板库里）必须照常可见，
    // 整页白屏会把"面板坏了"和"aria2 掉了"混成一回事。
    const s = useDownloadsStore();
    await s.load();
    expect(s.tasks).toHaveLength(1);
    expect(s.error).toBe('');
  });

  it('health.ok=false 保留 message 供横幅显示', async () => {
    boot({ health: { ok: false, message: 'connection refused' } });
    const s = useDownloadsStore();
    await s.load();
    expect(s.health?.ok).toBe(false);
    expect(s.health?.message).toContain('connection refused');
  });

  it('tasks 拉取失败时 error 带后端原文', async () => {
    boot({ tasksError: { message: '后端 503' } });
    const s = useDownloadsStore();
    await s.load();
    expect(s.error).toContain('503');
  });

  it('事件合并窗口：连发 3 个事件只重拉一次', async () => {
    const f = boot();
    const s = useDownloadsStore();
    await s.load();
    const before = f.taskFetches();
    s.applyEvent({ kind: 'paused', gid: 'x', at: 1 });
    s.applyEvent({ kind: 'paused', gid: 'y', at: 1 });
    s.applyEvent({ kind: 'completed', gid: 'z', at: 1 });
    await tick();
    // 三次事件 → 一次重拉（不含最初 load 的那次）。
    expect(f.taskFetches() - before).toBe(1);
  });

  it('pause 失败时 actionError 带原因，且不清空', async () => {
    boot({ postError: { message: 'aria2: task not active' } });
    const s = useDownloadsStore();
    await s.load();
    await s.pause('g1');
    expect(s.actionError).toContain('task not active');
  });

  it('remove(force) 把 force 传进 query', async () => {
    const f = boot();
    const s = useDownloadsStore();
    await s.load();
    await s.remove('g9', true);
    expect(f.dels).toContain('/api/dl/tasks/g9?force=true');
  });

  it('add 成功返回 true 并触发刷新', async () => {
    const f = boot();
    const s = useDownloadsStore();
    const ok = await s.add({ uris: ['https://a/b'] });
    expect(ok).toBe(true);
    expect(f.posts).toContain('/api/dl/tasks');
  });

  it('add 失败返回 false 且原因可见', async () => {
    boot({ postError: { message: '不支持的协议' } });
    const s = useDownloadsStore();
    const ok = await s.add({ uris: ['file:///etc/passwd'] });
    expect(ok).toBe(false);
    expect(s.actionError).toContain('不支持的协议');
  });

  it('clearHist 只打 history 端点', async () => {
    const f = boot();
    const s = useDownloadsStore();
    await s.load();
    const ok = await s.clearHist();
    expect(ok).toBe(true);
    expect(f.dels).toContain('/api/dl/history');
  });
});
