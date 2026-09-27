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
    exit_status: null,
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

  // 提示的"显示"归壳层 toast（toast.spec.ts 覆盖），本组件的契约只到
  // store.notice 为止：写对内容、别的什么都不做。之前这几条断言的是
  // 面板自己渲染的 .toast —— 那份 toast 有个 onUnmounted 里的
  // clearNotice，正好在跳转时把消息抹掉，是 bug 的一部分而不是特性。
  // 提示的"显示"归壳层 toast（toast.spec.ts 覆盖：显示、自动消失、
  // 同一句话第二次也要显示）。本组件的契约只到 store.notice：内容写对、
  // 除此之外不多做。面板里那份本地 toast 已经在带一个 bug —— 它的
  // onUnmounted 会 clearNotice，而点命令必然跳页、面板必然卸载，
  // 于是消息在壳层看到之前就被抹掉了。
  it('自动新建会话时把去了哪写进 notice（否则终端上那行字凭空冒出来）', async () => {
    // 标题照后端真实的取法写：自动新建的会话叫「快捷命令 · <命令名>」，
    // 所以提示里带上标题就等于带上了命令名。第一版我在这里断言提示里
    // 要有命令名、而 fake 给的是 'lp-4'，红的是我自己的假数据不真实，
    // 不是实现 —— 提示里再把命令名复述一遍只会变成
    // "已新建会话 快捷命令 · 看盘 执行 看盘"。
    const w = await mountPanel({
      run: { session_id: 4, is_new_session: true, title: '快捷命令 · 看盘' },
      busy: { 1: { busy: true, foreground: 'apt' } },
    });
    useTerminalStore().sessions = [termRow(1), termRow(4)];
    await w.find('.cmd-tile').trigger('click');
    await flushPromises();
    const qc = useQuickCmdStore();
    expect(qc.notice).toContain('快捷命令 · 看盘');
    expect(qc.notice).toContain('忙');
  });

  // 面板自己不渲染提示：两个消费者抢同一个 notice 字段，就会出现
  // "谁先取走谁说了算"，而跳转路上先取走的那个恰恰会把它吃掉。
  it('面板不渲染 toast（提示只有一个消费者：壳层）', async () => {
    const w = await mountPanel({
      run: { session_id: 1, is_new_session: true, title: 'lp-4' },
      busy: { 1: { busy: false } },
    });
    await w.find('.cmd-tile').trigger('click');
    await flushPromises();
    expect(w.find('.toast').exists()).toBe(false);
    expect(useQuickCmdStore().notice).not.toBe('');
  });

  it('执行失败时留在本页，并把原因写进 notice', async () => {
    const w = await mountPanel({ runError: Object.assign(new Error('注入失败'), { code: 'tmux_error' }) });
    await w.find('.cmd-tile').trigger('click');
    await flushPromises();
    // 不许跳到终端：那会让用户以为命令跑起来了。
    expect(router.currentRoute.value.name).toBe('quick');
    expect(useQuickCmdStore().notice).toContain('注入失败');
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

