import { describe, expect, it, vi, beforeEach } from 'vitest';
import { mount, flushPromises } from '@vue/test-utils';
import { createPinia, setActivePinia } from 'pinia';

import { setApi, resetApi } from '../api/inject';
import CommandsPanel from '../components/quickcmd/CommandsPanel.vue';
import { makeRouter } from './cmdpanelHarness';
import { useQuickCmdStore } from '../stores/quickcmd';
import { useTerminalStore } from '../stores/terminal';
import type { BusyEntry, CommandRow } from '../api/quickcmd';
import type { TermSessionRow } from '../api/terminal';

const location = { pathname: '/quick', search: '', assign: () => {} };

function row(over: Partial<CommandRow> = {}): CommandRow {
  return {
    id: 1, name: '看盘', command: 'df -h', cwd: '', need_confirm: false, sort: 10, created_at: 0, ...over,
  };
}
function termRow(id: number): TermSessionRow {
  return {
    id, title: `lp-${id}`, tmux_name: `lp-${id}`, cwd: '', shell: 'bash', created_at: 0,
    history_limit: 20000, last_attached_at: 0, alive: true,
  };
}

interface Opts {
  run?: unknown;
  runError?: unknown;
  busy?: Record<number, Partial<BusyEntry>>;
}

function fakeApi(o: Opts = {}) {
  const posts: { p: string; b?: unknown }[] = [];
  const dels: string[] = [];
  return {
    posts,
    dels,
    api: {
      get: vi.fn(async (p: string) => {
        if (p.startsWith('/api/commands/busy')) {
          const ids = [...p.matchAll(/session=(\d+)/g)].map((m) => Number(m[1]));
          return {
            sessions: ids
              .filter((id) => o.busy?.[id])
              .map((id) => ({ busy: false, foreground: '', shell_name: 'bash', ...o.busy![id], session_id: id })),
          };
        }
        if (p.startsWith('/api/term/sessions')) return { sessions: [termRow(1)] };
        return { commands: rows };
      }),
      post: vi.fn(async (p: string, b?: unknown) => {
        posts.push({ p, b });
        if (p.includes('/run')) {
          if (o.runError) throw o.runError;
          return o.run ?? { session_id: 1, is_new_session: false, title: 'lp-1' };
        }
        return row();
      }),
      patch: vi.fn(async () => row()),
      put: vi.fn(async () => ({})),
      del: vi.fn(async (p: string) => {
        dels.push(p);
        return { ok: true };
      }),
    },
  };
}

let rows: CommandRow[] = [row()];
let f: ReturnType<typeof fakeApi>;
let router: ReturnType<typeof makeRouter>;

async function mountPanel(o: Opts = {}) {
  setActivePinia(createPinia());
  resetApi();
  f = fakeApi(o);
  setApi(f.api as never, location);
  router = makeRouter();
  await router.push('/quick');
  await router.isReady();
  const w = mount(CommandsPanel, { global: { plugins: [router] }, attachTo: document.body });
  await flushPromises();
  return w;
}

beforeEach(() => {
  rows = [row()];
  vi.useFakeTimers({ toFake: ['setTimeout'] }); // 提示的消失计时不许把测试拖住
});

describe('CommandsPanel 列表', () => {
  it('挂载即拉列表并渲染磁贴', async () => {
    const w = await mountPanel();
    expect(w.findAll('.cmd-tile')).toHaveLength(1);
    expect(w.text()).toContain('看盘');
  });

  it('空列表给出可操作的空态，而不是一片空白', async () => {
    rows = [];
    const w = await mountPanel();
    expect(w.findAll('.cmd-tile')).toHaveLength(0);
    expect(w.text()).toContain('添加命令');
  });

  it('列表读不到时说明原因，不画一个空列表让用户白找', async () => {
    setActivePinia(createPinia());
    resetApi();
    setApi(
      {
        get: vi.fn(async () => {
          throw new Error('数据库被占用');
        }),
        post: vi.fn(), patch: vi.fn(), put: vi.fn(), del: vi.fn(),
      } as never,
      location,
    );
    router = makeRouter();
    await router.push('/quick');
    const w = mount(CommandsPanel, { global: { plugins: [router] } });
    await flushPromises();
    expect(w.find('.errline').text()).toContain('数据库被占用');
  });
});

describe('执行（D20）', () => {
  it('点一条命令 → 注入、跳终端页、选中对应标签', async () => {
    const w = await mountPanel({ busy: { 1: { busy: false } } });
    await w.find('.cmd-tile').trigger('click');
    await flushPromises();
    expect(f.posts.some((c) => c.p === '/api/commands/1/run')).toBe(true);
    // 停在命令页 = 用户以为没反应
    expect(router.currentRoute.value.name).toBe('term');
    // 只跳页不选标签 = 他盯着另一个会话等那行字出现
    expect(useTerminalStore().activeId).toBe(1);
  });

  it('need_confirm 的命令先弹确认框，确认前一个请求都不发', async () => {
    rows = [row({ need_confirm: true })];
    const w = await mountPanel();
    await w.find('.cmd-tile').trigger('click');
    await flushPromises();
    // 顺序是这条守卫的全部意义：先注入再弹窗，确认框就只是个通知
    expect(f.posts.filter((c) => c.p.includes('/run'))).toHaveLength(0);
    expect(w.find('.run-confirm').exists()).toBe(true);
    expect(w.find('.run-confirm').text()).toContain('df -h'); // 确认的是"这条命令"，不是"一个命令"

    await w.find('.run-confirm .btn-confirm').trigger('click');
    await flushPromises();
    // 不带 confirm=1 后端会 400：确认框点了却每次都失败
    expect(f.posts.find((c) => c.p.includes('/run'))?.p).toContain('confirm=1');
  });

  it('确认框点取消：不发请求，磁贴还能再点', async () => {
    rows = [row({ need_confirm: true })];
    const w = await mountPanel();
    await w.find('.cmd-tile').trigger('click');
    await flushPromises();
    await w.find('.run-confirm .btn-cancel').trigger('click');
    await flushPromises();
    expect(f.posts.filter((c) => c.p.includes('/run'))).toHaveLength(0);
    expect(w.find('.run-confirm').exists()).toBe(false);

    await w.find('.cmd-tile').trigger('click');
    await flushPromises();
    expect(w.find('.run-confirm').exists()).toBe(true);
  });

  it('自动新建会话时提示命令去了哪（否则终端上那行字凭空冒出来）', async () => {
    const w = await mountPanel({
      run: { session_id: 4, is_new_session: true, title: 'lp-4' },
      busy: { 1: { busy: true, foreground: 'apt' } },
    });
    useTerminalStore().sessions = [termRow(1), termRow(4)];
    await w.find('.cmd-tile').trigger('click');
    await flushPromises();
    expect(w.find('.toast').text()).toContain('lp-4');
  });

  // 提示说的事件已经过去（"已新建会话 lp-4"是刚才那一次的事）。
  // 常驻不消失会被读成"当前状态"，于是下一次点之前它还在说上一轮的话。
  // 用假计时器推进，不靠 sleep —— 真实延迟下这条测试会变成掷骰子。
  it('提示会自己消失', async () => {
    const w = await mountPanel({
      run: { session_id: 1, is_new_session: true, title: 'lp-4' },
      busy: { 1: { busy: false } },
    });
    await w.find('.cmd-tile').trigger('click');
    await flushPromises();
    expect(w.find('.toast').exists()).toBe(true);
    vi.advanceTimersByTime(3000);
    await flushPromises();
    expect(w.find('.toast').exists()).toBe(false);
  });

  // 每次都一样的文案最容易踩空：notice 若不清空，值不变就不会再触发
  // 显示逻辑，第二次点就没有任何提示 —— 而用户正因为看不见才连点。
  it('同一条提示连续出现两次，两次都要弹出来', async () => {
    const w = await mountPanel({
      run: { session_id: 1, is_new_session: true, title: 'lp-4' },
      busy: { 1: { busy: false } },
    });
    await w.find('.cmd-tile').trigger('click');
    await flushPromises();
    expect(w.find('.toast').exists()).toBe(true);
    vi.advanceTimersByTime(3000);
    await flushPromises();
    expect(w.find('.toast').exists()).toBe(false);

    await w.find('.cmd-tile').trigger('click');
    await flushPromises();
    expect(w.find('.toast').exists()).toBe(true);
  });

  it('执行失败时留在本页报错，不许跳到终端让用户以为成功了', async () => {
    const w = await mountPanel({ runError: Object.assign(new Error('注入失败'), { code: 'tmux_error' }) });
    await w.find('.cmd-tile').trigger('click');
    await flushPromises();
    expect(router.currentRoute.value.name).toBe('quick');
    expect(w.find('.toast').text()).toContain('注入失败');
  });
});

describe('增删改与排序', () => {
  it('点"添加命令"打开表单，保存后重拉列表并关掉抽屉', async () => {
    const w = await mountPanel();
    await w.find('.tile-add').trigger('click');
    expect(w.find('.addcmd').exists()).toBe(true);
    await w.find('[name=name]').setValue('新命令');
    await w.find('[name=command]').setValue('uptime');
    await w.find('.btn-save').trigger('click');
    await flushPromises();
    expect(f.posts.some((c) => c.p === '/api/commands')).toBe(true);
    expect(w.find('.addcmd').exists()).toBe(false);
  });

  it('点磁贴上的编辑打开同一个表单并回填', async () => {
    const w = await mountPanel();
    await w.find('.ib-edit').trigger('click');
    expect((w.find('[name=name]').element as HTMLInputElement).value).toBe('看盘');
  });

  it('删除走确认框，取消不发 DELETE', async () => {
    const w = await mountPanel();
    await w.find('.ib-del').trigger('click');
    expect(w.find('.del-confirm').exists()).toBe(true);
    await w.find('.del-confirm .btn-cancel').trigger('click');
    await flushPromises();
    expect(f.dels).toHaveLength(0);
  });

  it('确认删除才发 DELETE', async () => {
    const w = await mountPanel();
    await w.find('.ib-del').trigger('click');
    await w.find('.del-confirm .btn-confirm').trigger('click');
    await flushPromises();
    expect(f.dels).toEqual(['/api/commands/1']);
  });

  it('上移/下移带方向', async () => {
    const w = await mountPanel();
    await w.find('.mv-up').trigger('click');
    await flushPromises();
    expect(
      f.posts.some((c) => c.p === '/api/commands/1/move' && (c.b as { dir: string }).dir === 'up'),
    ).toBe(true);
  });
});

describe('忙闲预判（标在终端标签上，不标在命令上）', () => {
  it('按会话列表问一次忙闲，并给出"点了会落到哪"的提示', async () => {
    const w = await mountPanel({ busy: { 1: { busy: false } } });
    expect(w.find('.cmdhint').text()).toContain('直接执行');
    // 一次问一批：四个标签逐个请求就是四倍往返，而每次往返在 tmux 上都是 fork
    expect(f.api.get.mock.calls.filter((c) => String(c[0]).startsWith('/api/commands/busy')).length).toBe(1);
  });

  it('全会忙时提示会新建会话（D20）', async () => {
    const w = await mountPanel({ busy: { 1: { busy: true, foreground: 'apt' } } });
    expect(w.find('.cmdhint').text()).toContain('新建');
  });
});

