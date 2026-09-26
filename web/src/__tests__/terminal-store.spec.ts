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
  // dead 行不进 sessions 是 2026-09 的改动（真机反馈：退出的标签挂在
  // 标签栏上，要点进去才知道没了）。原来这条断言是 toEqual([7, 9])，
  // 保护的是旧策略。
  it('load 填充 sessions 并把 active 指向第一个', async () => {
    const { store } = boot({
      get: vi.fn(async () => ({ sessions: [row({ id: 7 }), row({ id: 9, alive: false })] })),
    });
    await store.load();
    expect(store.sessions.map((s) => s.id)).toEqual([7]);
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

  // TerminalView 把 reload 当作轮询的 before 用：每一轮先重排"会话清单"，
  // 再按新清单问 Busy。load 自己只记 error 不抛（否则别的调用方直接吃
  // rejection），而轮询那一侧要求"这一轮失败下一轮照跑"—— reload 就是
  // 这层壳。它容易被漏掉：漏了的话一次 500 就永久停表。
  it('reload：失败被吞掉，已有列表原样留着', async () => {
    let n = 0;
    const { store } = boot({
      get: async () => {
        n++;
        if (n === 1) return { sessions: [row()] };
        throw Object.assign(new Error('boom'), { status: 500, code: 'tmux_error' });
      },
    });
    await store.load();
    await expect(store.reload()).resolves.toBeUndefined();
    // 旧数据留着：清空的话，一次 500 就把整个标签栏抹成空面板，
    // 看起来像"会话全没了"。
    expect(store.sessions.map((x) => x.id)).toEqual([1]);
    // error 由 load 负责记（那边有用例），这里只保证它没把 reload 变成抛错。
    expect(store.error).not.toBe('');
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
    store.loaded = true; // "刚刚还在"的前提是之前载入过
    await store.reload();
    // 不再是 alive=false 的灰标签，而是整个摘掉 + 说清楚为什么
    expect(store.sessions).toEqual([]);
    expect(store.notice).toContain('会话 1');
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

// 会话在 tmux 里 exit 之后，标签必须自己消失（真机反馈）：之前它变成
// 灰色的"已退出"标签挂在标签栏上，用户要一个个点进去才知道没了。
//
// 只在"这个标签刚刚还在"时提示一句。第一次进页面看见库里躺着的历史死行
// 不提示 —— 那是几小时前退出的会话，弹一句"已退出"是在打扰。
describe('terminal store：退出的会话自动摘掉', () => {
  it('列表里没有 dead 标签，只留活着的', async () => {
    const { store } = boot({
      get: vi.fn(async () => ({ sessions: [row({ id: 1 }), row({ id: 2, alive: false })] })),
    });
    await store.load();
    expect(store.sessions.map((s) => s.id)).toEqual([1]);
  });

  // 用两轮 load 而不是手摆 state：真实场景就是轮询两轮之间少了一个标签，
  // 手摆 state 会绕过 loaded 这个前提，测的就不是同一件事。
  it('刚刚还在的标签消失了，提示是哪几个', async () => {
    let round = 0;
    const { store } = boot({
      get: vi.fn(async () => ({
        sessions:
          round++ === 0
            ? [row({ id: 1, title: '看盘' }), row({ id: 2, title: '跑任务' })]
            : [row({ id: 1, title: '看盘' })],
      })),
    });
    await store.load();
    expect(store.notice).toBe(''); // 第一轮：谁都没少
    await store.load();
    expect(store.notice).toContain('跑任务');
    expect(store.notice).not.toContain('看盘');
  });

  it('首次载入不提示（几小时前退出的会话不该弹提示）', async () => {
    const { store } = boot({
      get: vi.fn(async () => ({ sessions: [row({ id: 2, alive: false })] })),
    });
    await store.load();
    expect(store.notice).toBe('');
    expect(store.sessions).toEqual([]);
  });

  // 正在看的会话退出：activeId 必须落到还活着的那个，不能停在一个
  // 已经不存在的 id 上（那会让屏幕停在空白页，而别的标签其实是好的）。
  it('当前会话退出后 active 落到活着的下一个', async () => {
    const { store } = boot({
      get: vi.fn(async () => ({ sessions: [row({ id: 1 }), row({ id: 2, alive: false })] })),
    });
    store.sessions = [row({ id: 1 }), row({ id: 2 })];
    store.loaded = true;
    store.activeId = 2;
    await store.load();
    expect(store.activeId).toBe(1);
  });
});
