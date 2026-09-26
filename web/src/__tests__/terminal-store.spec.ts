import { beforeEach, describe, expect, it, vi } from 'vitest';
import { setActivePinia, createPinia } from 'pinia';
import { useTerminalStore, autoTitle, errorMessage } from '../stores/terminal';
import { setApi, resetApi } from '../api/inject';
import type { TermSessionRow } from '../api/terminal';

function row(over: Partial<TermSessionRow> = {}): TermSessionRow {
  return {
    id: 1,
    tmux_name: 'lp-1',
    title: '会话 1',
    cwd: '/root',
    shell: '/bin/bash',
    history_limit: 20000,
    created_at: 1_700_000_000,
    last_attached_at: 0,
    alive: true,
    ...over,
  };
}

// 只实现终端 store 用到的方法：多写用不到的方法，就等于替它
// 预先保证「它不会调用别的东西」——那不该由这个测试来担保。
function fakeApi(over: Record<string, any> = {}) {
  return {
    get: vi.fn(async () => ({ sessions: [] as TermSessionRow[] })),
    post: vi.fn(async () => ({ session: row() })),
    patch: vi.fn(async () => ({ session: row() })),
    del: vi.fn(async () => ({})),
    ...over,
  } as any;
}

function boot(over: Record<string, any> = {}) {
  const api = fakeApi(over);
  setApi(api, { pathname: '/term', search: '', assign: vi.fn() });
  setActivePinia(createPinia());
  return { store: useTerminalStore(), api };
}

beforeEach(() => resetApi());

describe('autoTitle', () => {
  it('有现名时用现名', () => {
    expect(autoTitle('我的编译', [{ title: '我的编译' }])).toBe('我的编译');
  });

  it('无现名时取下一个可用序号', () => {
    expect(autoTitle('', [{ title: '会话 1' }])).toBe('会话 2');
    expect(autoTitle('', [{ title: '会话 1' }, { title: '会话 3' }])).toBe('会话 2');
    expect(autoTitle('', [])).toBe('会话 1');
  });

  // 撞名不报错，只会让标签栏出现两个"会话 2"：用户分不清哪个是哪个，
  // 于是把正在跑任务的那个关掉。
  it('跳过一个已被占用的序号', () => {
    expect(autoTitle('', [{ title: '会话 2' }])).toBe('会话 1');
    expect(autoTitle('', [{ title: '会话 1' }, { title: '会话 2' }])).toBe('会话 3');
  });

  // 用户自定义的标题里也可能有"会话"字样，不能把它当编号统计，
  // 否则 "会话 999999" 会让我们下次从 1000000 开始编。
  it('只统计自动生成的标题', () => {
    expect(autoTitle('', [{ title: '会话 旧机器迁移' }, { title: '会话 2' }])).toBe('会话 1');
  });
});

describe('terminal store：列表', () => {
  it('load 填充 sessions 并把 active 指向第一个', async () => {
    const { store } = boot({
      get: vi.fn(async () => ({ sessions: [row({ id: 7 }), row({ id: 9, alive: false })] })),
    });
    await store.load();
    expect(store.sessions.map((s) => s.id)).toEqual([7, 9]);
    expect(store.activeId).toBe(7);
    expect(store.loaded).toBe(true);
    expect(store.error).toBe('');
  });

  // 后端空列表可能返回 null；直接 .map 会整块炸掉视图。
  it('sessions 为 null 时不炸', async () => {
    const { store } = boot({ get: vi.fn(async () => ({ sessions: null })) });
    await store.load();
    expect(store.sessions).toEqual([]);
  });

  it('load 失败时记下错误而不抛出', async () => {
    const { store } = boot({ get: vi.fn(async () => { throw { message: '连不上' }; }) });
    await store.load();
    expect(store.error).toContain('连不上');
    expect(store.loaded).toBe(false);
  });

  // 终端模块没装（没装 tmux）时后端返回 501；这个状态必须能被视图区分，
  // 否则会显示成"你还没有会话"，用户会一直去找新建按钮。
  it('501 时记为不可用', async () => {
    const { store } = boot({ get: vi.fn(async () => { throw { status: 501, message: '终端模块未启用' }; }) });
    await store.load();
    expect(store.unavailable).toBe(true);
    expect(store.error).toContain('终端模块未启用');
  });
});

describe('terminal store：新建', () => {
  it('create 后把新会话追加进列表并设为 active', async () => {
    const { store, api } = boot({ post: vi.fn(async () => ({ session: row({ id: 42, title: '会话 3' }) })) });
    store.sessions = [row({ id: 1 })];
    await store.create({ title: '' });
    expect(api.post).toHaveBeenCalledWith('/api/term/sessions', { title: '会话 2' });
    expect(store.sessions.map((s) => s.id)).toEqual([1, 42]);
    expect(store.activeId).toBe(42);
  });

  it('创建失败时不动列表、不改 active', async () => {
    const { store } = boot({ post: vi.fn(async () => { throw { message: 'tmux 没装' }; }) });
    store.sessions = [row({ id: 1 })];
    store.activeId = 1;
    await expect(store.create({ title: 'x' })).rejects.toBeTruthy();
    expect(store.sessions.map((s) => s.id)).toEqual([1]);
    expect(store.activeId).toBe(1);
  });
});

describe('terminal store：改名与删除', () => {
  it('rename 成功后就地更新标题', async () => {
    const { store, api } = boot({ patch: vi.fn(async () => ({ session: row({ id: 1, title: '新名' }) })) });
    store.sessions = [row({ id: 1, title: '旧名' })];
    await store.rename(1, '新名');
    expect(api.patch).toHaveBeenCalledWith('/api/term/sessions/1', { title: '新名' });
    expect(store.sessions[0].title).toBe('新名');
  });

  // 改名失败却留着新标题 = 界面在骗人；下次重连后它又变回旧名，
  // 用户以为自己记错了。
  it('rename 失败时保留原标题', async () => {
    const { store } = boot({ patch: vi.fn(async () => { throw { message: '会话不存在' }; }) });
    store.sessions = [row({ id: 1, title: '旧名' })];
    await expect(store.rename(1, '新名')).rejects.toBeTruthy();
    expect(store.sessions[0].title).toBe('旧名');
    expect(store.error).toContain('会话不存在');
  });

  // 删除必须带 confirm=1：只在前端弹确认框的话，脚本或漏改的调用
  // 会把正在跑任务的会话连 shell 一起杀掉。
  it('remove 走带 confirm 的路径', async () => {
    const { store, api } = boot();
    store.sessions = [row({ id: 1 }), row({ id: 2 })];
    store.activeId = 1;
    await store.remove(1);
    expect(api.del).toHaveBeenCalledWith('/api/term/sessions/1?confirm=1');
  });

  it('删除当前会话后 active 落到剩下的那个', async () => {
    const { store } = boot();
    store.sessions = [row({ id: 1 }), row({ id: 2 })];
    store.activeId = 1;
    await store.remove(1);
    expect(store.sessions.map((s) => s.id)).toEqual([2]);
    expect(store.activeId).toBe(2);
  });

  it('删除最后一个后 active 归零', async () => {
    const { store } = boot();
    store.sessions = [row({ id: 1 })];
    store.activeId = 1;
    await store.remove(1);
    expect(store.activeId).toBe(0);
  });

  // 删除失败绝不能把行从列表里拿掉：那会变成"面板以为删了，
  // 实际 tmux 里还在跑"，而且用户再也点不到它。
  it('删除失败时列表原样保留', async () => {
    const { store } = boot({ del: vi.fn(async () => { throw { message: '删不掉' }; }) });
    store.sessions = [row({ id: 1 })];
    store.activeId = 1;
    await expect(store.remove(1)).rejects.toBeTruthy();
    expect(store.sessions.map((s) => s.id)).toEqual([1]);
    expect(store.activeId).toBe(1);
  });
});

// 面板是多人/多标签共用一台机器的：别的标签（或别人）建的会话，
// 这里必须能看到，否则标签栏会骗人说"没有会话"。
describe('terminal store：外部变更', () => {
  it('reload 会重新拉取列表', async () => {
    const { store, api } = boot({
      get: vi
        .fn()
        .mockResolvedValueOnce({ sessions: [row({ id: 1 })] })
        .mockResolvedValueOnce({ sessions: [row({ id: 1 }), row({ id: 2 })] }),
    });
    await store.load();
    await store.reload();
    expect(api.get).toHaveBeenCalledTimes(2);
    expect(store.sessions.map((s) => s.id)).toEqual([1, 2]);
  });

  // 在 tmux 里自己 kill-session（或会话被 OOM 干掉）之后，面板上那个
  // 标签还挂着绿点，点进去才报"会话不存在"。刷新要有一个明确的入口。
  it('refreshAlive 重拉后同步存活状态', async () => {
    const { store, api } = boot({
      get: vi.fn(async () => ({ sessions: [row({ id: 1, alive: false })] })),
    });
    store.sessions = [row({ id: 1, alive: true })];
    await store.reload();
    expect(store.sessions[0].alive).toBe(false);
    expect(api.get).toHaveBeenCalled();
  });

  // 重拉不能把 active 弄丢：用户正在那个标签里打字，列表一刷新
  // 就跳回第一个，等于把屏幕从别人手上抢走。
  it('reload 保留当前 active', async () => {
    const { store } = boot({
      get: vi.fn(async () => ({ sessions: [row({ id: 1 }), row({ id: 2 })] })),
    });
    await store.load();
    store.activeId = 2;
    await store.reload();
    expect(store.activeId).toBe(2);
  });

  it('active 指向已被删掉的会话时归零', async () => {
    const { store } = boot({
      get: vi.fn(async () => ({ sessions: [row({ id: 2 })] })),
    });
    store.sessions = [row({ id: 1 })];
    store.activeId = 1;
    await store.reload();
    expect(store.activeId).toBe(2);
  });
});

describe('errorMessage', () => {
  it('优先用后端给的文案', () => {
    expect(errorMessage({ message: '标题不能为空' })).toBe('标题不能为空');
  });
  it('没有文案时给兜底', () => {
    expect(errorMessage(new Error(''))).not.toBe('');
  });
});
