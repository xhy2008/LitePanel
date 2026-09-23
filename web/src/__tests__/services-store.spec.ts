import { describe, it, expect, beforeEach, vi } from 'vitest';
import { setActivePinia, createPinia } from 'pinia';
import { setApi, resetApi } from '../api/inject';
import { useServicesStore, describeService } from '../stores/services';
import type { ServiceRow } from '../api/services';

// 后端一行数据的形状在这里声明一次；测试与实现共用，
// 字段名漂移会立刻编译不过（前端不做 camelCase 转换）。
function row(over: Partial<ServiceRow> = {}): ServiceRow {
  return {
    id: 1,
    name: 'llama-server',
    kind: 'command',
    unit: '',
    start_cmd: 'sleep 1',
    stop_cmd: '',
    cwd: '',
    autostart: false,
    sort: 0,
    created_at: 0,
    state: 'running',
    pid: 41287,
    started_at: Math.floor(Date.now() / 1000) - 7200,
    ...over,
  };
}

function fakeApi(rows: ServiceRow[] = []) {
  const calls: string[] = [];
  const api = {
    get: vi.fn(async (path: string) => {
      calls.push('GET ' + path);
      if (path.startsWith('/api/services') && !path.includes('/log')) {
        return { services: rows };
      }
      return { lines: [], cached_lines: 0, buffer_limit: 500 };
    }),
    post: vi.fn(async (path: string) => {
      calls.push('POST ' + path);
      if (path.endsWith('/toggle')) {
        return row({ state: 'stopped', pid: 0, exit_reason: 'clean', exit_code: 0, stopped_by: 'user' });
      }
      return { ok: true };
    }),
    patch: vi.fn(async () => rows[0]),
    put: vi.fn(async () => ({})),
    del: vi.fn(async () => ({ ok: true })),
  };
  return { api, calls };
}

beforeEach(() => {
  setActivePinia(createPinia());
  resetApi();
});

// ---- D21：退出呈现只有三种 ----

describe('describeService（D21 三态）', () => {
  it('运行中显示 PID 与运行时长', () => {
    const d = describeService(row({ started_at: Math.floor(Date.now() / 1000) - 7325 }));
    expect(d.tone).toBe('run');
    expect(d.text).toContain('PID 41287');
    expect(d.text).toContain('2 小时');
  });

  it('正常退出显示 code 0', () => {
    const d = describeService(
      row({ state: 'stopped', pid: 0, exit_reason: 'clean', exit_code: 0, exit_at: Math.floor(Date.now() / 1000) }),
    );
    expect(d.tone).toBe('good');
    expect(d.text).toContain('正常退出');
    expect(d.text).toContain('code 0');
  });

  it('异常退出显示 code N', () => {
    const d = describeService(
      row({ state: 'stopped', pid: 0, exit_reason: 'error', exit_code: 1 }),
    );
    expect(d.tone).toBe('bad');
    expect(d.text).toContain('异常退出');
    expect(d.text).toContain('code 1');
  });

  // exit_reason 缺席 = 从没退出过。把它当"正常退出 code 0"会让用户
  // 以为服务跑过一遍又好好退了，实际是一次都没启动。
  it('从没退出过不能显示成"正常退出 code 0"', () => {
    const d = describeService(row({ state: 'stopped', pid: 0 }));
    expect(d.tone).toBe('idle');
    expect(d.text).not.toContain('code 0');
    expect(d.text).not.toContain('正常退出');
  });

  it('starting / stopping 有自己的文案，不伪装成运行中', () => {
    expect(describeService(row({ state: 'starting' })).tone).toBe('run');
    expect(describeService(row({ state: 'starting' })).text).toContain('启动中');
    expect(describeService(row({ state: 'stopping' })).text).toContain('停止中');
  });

  it('systemd 类型标 SYSV 徽标，command 标 CMD', () => {
    expect(describeService(row({ kind: 'command' })).badge).toBe('CMD');
    expect(describeService(row({ kind: 'systemd', unit: 'nginx' })).badge).toBe('SYSV');
  });
});

// ---- store ----

describe('services store', () => {
  it('load 填充列表', async () => {
    const { api } = fakeApi([row(), row({ id: 2, name: 'aria2' })]);
    setApi(api as any, { pathname: '/', search: '', assign: () => {} });
    const s = useServicesStore();
    await s.load();
    expect(s.items.map((i) => i.name)).toEqual(['llama-server', 'aria2']);
    expect(s.loaded).toBe(true);
  });

  // 后端空列表返回 []；万一返回 null，前端 .map 会炸整个视图。
  it('后端返回 null 时不炸', async () => {
    const api = { ...fakeApi().api, get: vi.fn(async () => ({ services: null })) };
    setApi(api as any, { pathname: '/', search: '', assign: () => {} });
    const s = useServicesStore();
    await s.load();
    expect(s.items).toEqual([]);
  });

  // 点开关要立刻有反馈：等一次完整 HTTP 往返才动，慢机上像"没响应"，
  // 用户就会连点，而连点会真的把服务反复启停。
  it('toggle 先乐观改状态', async () => {
    let resolvePost: (v: ServiceRow) => void = () => {};
    const { api } = fakeApi([row({ state: 'stopped', pid: 0 })]);
    api.post = vi.fn(
      () => new Promise((r) => (resolvePost = r)) as Promise<ServiceRow>,
    );
    setApi(api as any, { pathname: '/', search: '', assign: () => {} });
    const s = useServicesStore();
    await s.load();

    const p = s.toggle(1);
    expect(s.items[0].state).toBe('starting');
    expect(s.pending[1]).toBe(true);

    resolvePost(row({ state: 'running', pid: 99 }));
    await p;
    expect(s.items[0].pid).toBe(99);
    expect(s.pending[1]).toBeFalsy();
  });

  // 失败必须回滚：留着乐观状态就是骗用户说服务在跑。
  it('toggle 失败回滚原状态', async () => {
    const { api } = fakeApi([row({ state: 'stopped', pid: 0 })]);
    api.post = vi.fn(async () => {
      throw { code: 'start_failed', message: '启动失败', status: 409 };
    });
    setApi(api as any, { pathname: '/', search: '', assign: () => {} });
    const s = useServicesStore();
    await s.load();

    await expect(s.toggle(1)).rejects.toBeTruthy();
    expect(s.items[0].state).toBe('stopped');
    expect(s.pending[1]).toBeFalsy();
    expect(s.error).toContain('启动失败');
  });

  // 连点保护：一次请求没结束前不该再发第二次。
  it('pending 期间重复 toggle 不再发请求', async () => {
    let resolvePost: (v: ServiceRow) => void = () => {};
    const { api } = fakeApi([row({ state: 'stopped', pid: 0 })]);
    api.post = vi.fn(
      () => new Promise((r) => (resolvePost = r)) as Promise<ServiceRow>,
    );
    setApi(api as any, { pathname: '/', search: '', assign: () => {} });
    const s = useServicesStore();
    await s.load();

    const p = s.toggle(1);
    await s.toggle(1);
    expect(api.post).toHaveBeenCalledTimes(1);
    resolvePost(row({ state: 'running' }));
    await p;
  });

  // WS 事件要就地合并，否则状态只能靠重新拉列表，服务崩了 UI 会
  // 一直显示"运行中"直到下次刷新。
  it('applyEvent 就地合并 WS 状态', async () => {
    const { api } = fakeApi([row({ state: 'running', pid: 10 })]);
    setApi(api as any, { pathname: '/', search: '', assign: () => {} });
    const s = useServicesStore();
    await s.load();

    s.applyEvent({ id: 1, state: 'stopped', pid: 0, exit: { code: 137, signal: 9, reason: 'error', at: 1, stopped_by: 'pdeathsig' } });
    expect(s.items[0].state).toBe('stopped');
    expect(s.items[0].pid).toBe(0);
    expect(s.items[0].exit_reason).toBe('error');
    expect(s.items[0].exit_code).toBe(137);
    // 合并不得触发重新拉列表（那会覆盖别的服务的实时状态）。
    expect(api.get).toHaveBeenCalledTimes(1);
  });

  it('applyEvent 遇到未知 id 时重拉列表', async () => {
    const { api } = fakeApi([row()]);
    setApi(api as any, { pathname: '/', search: '', assign: () => {} });
    const s = useServicesStore();
    await s.load();
    s.applyEvent({ id: 999, state: 'running' });
    // 新建/删除的服务只能靠重拉出现在列表里。
    await vi.waitFor(() => expect(api.get).toHaveBeenCalledTimes(2));
  });

  it('create / remove 调对应接口并刷新', async () => {
    const { api } = fakeApi();
    setApi(api as any, { pathname: '/', search: '', assign: () => {} });
    const s = useServicesStore();
    await s.create({ name: 'x', kind: 'command', start_cmd: 'sleep 1' });
    expect(api.post).toHaveBeenCalledWith('/api/services', {
      name: 'x', kind: 'command', start_cmd: 'sleep 1',
    });
    await s.remove(3);
    expect(api.del).toHaveBeenCalledWith('/api/services/3');
  });
});
