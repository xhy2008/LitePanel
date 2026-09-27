import { describe, it, expect, vi, beforeEach } from 'vitest';
import { setActivePinia, createPinia } from 'pinia';
import { setApi, resetApi } from '../api/inject';
import { useQuickCmdStore, describeRun, tabBadge } from '../stores/quickcmd';
import { useTerminalStore } from '../stores/terminal';
import type { TermSessionRow } from '../api/terminal';
import type { BusyEntry, CommandRow } from '../api/quickcmd';

// 与 internal/quickcmd.Command 的 JSON tag 逐字段对齐（前端不做 camelCase
// 转换：一改名就会与 HTTP 返回的字段悄悄漂移）。
function row(over: Partial<CommandRow> = {}): CommandRow {
  return {
    id: 1,
    name: '磁盘占用排行',
    command: 'du -sh /data/* | sort -h',
    cwd: '',
    need_confirm: false,
    sort: 10,
    created_at: 0,
    ...over,
  };
}

interface Opts {
  runResult?: unknown;
  runError?: unknown;
  busy?: Record<number, Partial<BusyEntry>>;
  busyError?: unknown;
}

function fakeApi(rows: CommandRow[], o: Opts = {}) {
  const calls: { m: string; p: string; b?: unknown }[] = [];
  const api = {
    get: vi.fn(async (path: string) => {
      calls.push({ m: 'GET', p: path });
      if (path.startsWith('/api/commands/busy')) {
        if (o.busyError) throw o.busyError;
        const q = new URL('http://x' + path).searchParams;
        const ids = (q.getAll('session') || []).map(Number);
        return {
          sessions: ids
            .filter((id) => o.busy?.[id])
            .map((id) => ({
              busy: false,
              foreground: '',
              shell_name: 'bash',
              ...o.busy![id],
              session_id: id,
            }))
        };
      }
      return { commands: rows };
    }),
    post: vi.fn(async (path: string, body?: unknown) => {
      calls.push({ m: 'POST', p: path, b: body });
      if (path.includes('/run')) {
        if (o.runError) throw o.runError;
        return o.runResult ?? { session_id: 7, is_new_session: false, title: 'lp-3' };
      }
      return row();
    }),
    patch: vi.fn(async (path: string, body?: unknown) => {
      calls.push({ m: 'PATCH', p: path, b: body });
      return row();
    }),
    put: vi.fn(async () => ({})),
    del: vi.fn(async (path: string) => {
      calls.push({ m: 'DELETE', p: path });
      return { ok: true };
    }),
  };
  return { api, calls };
}

function termRow(id: number): TermSessionRow {
  return {
    id, title: `lp-${id}`, cwd: '', shell: 'bash', created_at: 0, tmux_name: `lp-${id}`,
    history_limit: 20000, last_attached_at: 0, alive: true,
    exit_status: null,
  };
}

function busy(o: Partial<BusyEntry> = {}): BusyEntry {
  return { session_id: 1, busy: false, foreground: '', shell_name: 'bash', ...o };
}

let api = fakeApi([]);
let location = { pathname: '/', search: '', assign: vi.fn() };

beforeEach(() => {
  setActivePinia(createPinia());
  resetApi();
  api = fakeApi([row()]);
  location = { pathname: '/', search: '', assign: vi.fn() };
  setApi(api.api as never, location as never);
});

describe('快捷命令 store', () => {
  it('load 拉列表；null 也要当空列表', async () => {
    api = fakeApi([{ ...row(), id: 9 }]);
    (api.api.get as never as ReturnType<typeof vi.fn>).mockResolvedValueOnce({ commands: null });
    setApi(api.api as never, location as never);
    const s = useQuickCmdStore();
    await(expect(s.load()));
    expect(s.items).toEqual([]);
  });

  it('create 提交名称/命令/cwd，成功后刷新列表', async () => {
    const s = useQuickCmdStore();
    await s.create({ name: '看盘', command: 'df -h', cwd: '/tmp' });
    const call = api.calls.find((c) => c.m === 'POST' && c.p === '/api/commands');
    expect(call?.b).toMatchObject({ name: '看盘', command: 'df -h', cwd: '/tmp' });
    expect(api.calls.filter((c) => c.p === '/api/commands' && c.m === 'GET').length).toBeGreaterThan(0);
  });

  it('remove 删除后刷新', async () => {
    const s = useQuickCmdStore();
    s.items = [row({ id: 3 })];
    await s.remove(3);
    expect(api.calls.some((c) => c.m === 'DELETE' && c.p === '/api/commands/3')).toBe(true);
  });

  it('move 的字段名必须是 dir：后端 JSON 解码拒绝未知字段', async () => {
    const s = useQuickCmdStore();
    await s.move(4, 'up');
    const call = api.calls.find((c) => c.p === '/api/commands/4/move');
    expect(call).toMatchObject({ m: 'POST', b: { dir: 'up' } });
  });

  // 后端的 PATCH 走的是与 POST 同样的必填校验，且拒绝未知字段：
  // 只发改动的几个字段会被"名称必填"挡掉，多发一个 sort 会被 400 挡掉。
  it('update 交完整字段（name/command/cwd/need_confirm）', async () => {
    const s = useQuickCmdStore();
    await s.update(2, { name: '新名', command: 'ls', cwd: '', need_confirm: true });
    const call = api.calls.find((c) => c.m === 'PATCH');
    expect(call?.b).toEqual({ name: '新名', command: 'ls', cwd: '', need_confirm: true });
  });
});

describe('run：执行 + 落地终端页', () => {
  it('跳终端页并把 store 指向注入后的那个会话', async () => {
    api = fakeApi([row()], { runResult: { session_id: 9, is_new_session: false, title: 'lp-9' } });
    setApi(api.api as never, location as never);
    const s = useQuickCmdStore();
    // 真用 terminal store，而不是手搓 {select: vi.fn()} 替身：替身只能证明
    // "store 喊了一声 select"，证明不了用户看到的就是那个会话。
    const term = useTerminalStore();
    term.sessions = [termRow(1), termRow(9)];
    term.activeId = 1;
    const go = vi.fn();
    await s.run(row(), { goTerm: go });

    // 两件必须同时做到的事：
    expect(go).toHaveBeenCalled(); // 只选标签不跳页，用户会停在命令页以为没反应
    expect(term.activeId).toBe(9); // 只跳页不选标签，他看到别的会话"凭空多出字"
  });

  it('自动新建会话时提示用户命令去了哪（否则终端上那行字凭空冒出来）', async () => {
    api = fakeApi([row()], { runResult: { session_id: 5, is_new_session: true, title: 'lp-7' } });
    setApi(api.api as never, location as never);
    const s = useQuickCmdStore();
    const term = useTerminalStore();
    term.sessions = [termRow(5)];
    await s.run(row(), { goTerm: vi.fn() });
    expect(s.notice).toContain('lp-7');
    expect(s.notice).toContain('忙');
  });

  it('未新建会话时不留提示', async () => {
    const s = useQuickCmdStore();
    const term = useTerminalStore();
    term.sessions = [termRow(7)];
    await s.run(row(), { goTerm: vi.fn() });
    expect(s.notice).toBe('');
  });

  it('409 会话忙：提示 + 不跳页 —— 绝不假装执行过', async () => {
    const err = Object.assign(new Error('目标终端会话都处于忙状态'), { status: 409, code: 'session_busy' });
    api = fakeApi([row()], { runError: err });
    setApi(api.api as never, location as never);
    const s = useQuickCmdStore();
    const term = useTerminalStore();
    term.sessions = [termRow(1)];
    const go = vi.fn();
    // 先让忙闲图样停在"空闲"上 —— 这正是用户会点下去的原因。
    api = fakeApi([row()], { busy: { 1: { busy: false } }, runError: busyErr() });
    setApi(api.api as never, location as never);
    await s.refreshBusy();
    expect(s.busy[1].busy).toBe(false);

    await expect(s.run(row(), { goTerm: go })).rejects.toBeTruthy();
    expect(go).not.toHaveBeenCalled(); // 绝不假装执行过
    expect(s.notice).toContain('忙');
    // 后端刚亲口说会话忙：本地那份"空闲"图样已被证伪，必须重取。
    // 不重取的话磁贴继续挂着"空闲"，用户会照着它一直点、一直失败。
    expect(api.calls.filter((c) => c.p.startsWith('/api/commands/busy')).length).toBe(2);
  });

  function busyErr() {
    return Object.assign(new Error('目标终端会话都处于忙状态'), { status: 409, code: 'session_busy' });
  }

  it('confirm_required：提示而不是当成"失败"这种通用字眼', async () => {
    const err = Object.assign(new Error('该命令被判定为危险命令，请确认后重试'), {
      status: 400,
      code: 'confirm_required',
    });
    api = fakeApi([row()], { runError: err });
    setApi(api.api as never, location as never);
    const s = useQuickCmdStore();
    await expect(s.run(row({ need_confirm: true }))).rejects.toBeTruthy();
    expect(s.notice).toContain('确认');
  });

  it('同一条命令在飞时重复点不再发请求', async () => {
    let resolve!: (v: unknown) => void;
    (api.api.post as never as ReturnType<typeof vi.fn>).mockImplementationOnce(
      () => new Promise((r) => (resolve = r)),
    );
    setApi(api.api as never, location as never);
    const s = useQuickCmdStore();
    const term = useTerminalStore();
    term.sessions = [termRow(7)];
    const opts = { goTerm: vi.fn() };
    const first = s.run(row({ id: 2 }), opts);
    const second = await s.run(row({ id: 2 }), opts);
    expect(second).toBe(false); // 被去重挡掉
    expect((api.api.post as never as ReturnType<typeof vi.fn>).mock.calls.length).toBe(1);
    resolve({ session_id: 7, is_new_session: false, title: 'lp-1' });
    await expect(first).resolves.toBe(true);
  });

  it('confirm 走 ?confirm=1', async () => {
    const s = useQuickCmdStore();
    await s.run(row({ need_confirm: true }), { confirm: true, goTerm: vi.fn() });
    const call = api.calls.find((c) => c.p.includes('/run'));
    expect(call?.p).toContain('confirm=1');
  });

  it('后端说会话 9，但本地没有那个标签：先重拉列表再选中', async () => {
    // 面板刚新建过会话而前端还没刷新时会发生。只选不拉就选不上，
    // 用户看到的是"命令执行了，可屏幕上什么都没多"。
    api = fakeApi([row()], { runResult: { session_id: 9, is_new_session: true, title: 'lp-9' } });
    setApi(api.api as never, location as never);
    const s = useQuickCmdStore();
    const term = useTerminalStore();
    term.sessions = [termRow(1)];
    (api.api.get as never as ReturnType<typeof vi.fn>).mockImplementationOnce(async () => ({
      sessions: [termRow(1), termRow(9)],
    }));
    await s.run(row(), { goTerm: vi.fn() });
    expect(term.activeId).toBe(9);
  });

  it('终端一个会话都没有时也要跳过去：那边会自己把新建入口摆出来', async () => {
    const s = useQuickCmdStore();
    const go = vi.fn();
    await s.run(row(), { goTerm: go });
    expect(go).toHaveBeenCalled();
  });
});

describe('busy：忙闲状态与终端 store 同步', () => {
  it('后端没答的会话不画任何标记，但按钮照样能点', async () => {
    // 关键在"没答 ≠ 空闲"：画成空闲是在骗用户，把结论摆出来就等于
    // 面板替 tmux 作了保；而禁用按钮又会让 tmux 抽风一下就把所有
    // 快捷命令变成点不动。两边都不许：既不骗，也不瘫。
    const s = useQuickCmdStore();
    const term = useTerminalStore();
    term.sessions = [termRow(1), termRow(2)];
    api = fakeApi([row()], { busy: { 1: { busy: false, shell_name: 'bash' } } }); // 只答了 1
    setApi(api.api as never, location as never);
    await s.refreshBusy();
    expect(s.busy[1].busy).toBe(false);
    expect(s.busy[2]).toBeUndefined(); // 后端没答 2：就是不知道
    await expect(s.run(row(), { goTerm: vi.fn() })).resolves.toBe(true);
  });

  it('按终端 store 现有会话批量查一次，结果落在 store 上', async () => {
    api = fakeApi([row()], { busy: { 1: { busy: true, foreground: 'apt', shell_name: 'bash' } } });
    setApi(api.api as never, location as never);
    const s = useQuickCmdStore();
    const term = useTerminalStore();
    term.sessions = [termRow(1), termRow(2)];
    await s.refreshBusy();
    expect(s.busy[1].busy).toBe(true);
    expect(s.busy[1].foreground).toBe('apt');
    // 一次问一批：逐个请求在 tmux 上就是每次一个 fork
    expect(api.calls.filter((c) => c.p.startsWith('/api/commands/busy')).length).toBe(1);
    expect(api.calls.find((c) => c.p.startsWith('/api/commands/busy'))!.p).toContain('session=1');
    expect(api.calls.find((c) => c.p.startsWith('/api/commands/busy'))!.p).toContain('session=2');
  });

  it('查不到就不画任何忙闲标记，但按钮照样能点', async () => {
    // 这里的关键是"查不到 ≠ 空闲"：画成空闲是在骗用户，
    // 而禁用按钮会让 tmux 抽风一下就把所有快捷命令变成点不动。
    // 两边都不许：既不骗，也不瘫。
    api = fakeApi([row()], { busyError: new Error('boom') });
    setApi(api.api as never, location as never);
    const s = useQuickCmdStore();
    const term = useTerminalStore();
    term.sessions = [termRow(1)];
    await s.refreshBusy();
    expect(s.busy).toEqual({});
    await expect(s.run(row(), { goTerm: vi.fn() })).resolves.toBe(true);
  });

  it('没有会话时不发请求', async () => {
    const s = useQuickCmdStore();
    await s.refreshBusy();
    expect(api.calls.some((c) => c.p.startsWith('/api/commands/busy'))).toBe(false);
  });
});

describe('describeRun（忙闲文案）', () => {
  it('忙：给出前台进程名（"在跑什么"决定该不该等）', () => {
    expect(describeRun(busy({ busy: true, foreground: 'apt' }))).toContain('apt');
  });

  it('空闲：说清楚会直接投进这个会话', () => {
    const t = describeRun(busy({ busy: false, foreground: 'bash' }));
    expect(t).toContain('空闲');
    expect(t).not.toContain('bash'); // 前台就是 shell 本身，没必要当"进程名"显示
  });

  it('未知：不含"忙"也不含"空闲"，避免读成确定结论', () => {
    const t = describeRun(undefined);
    expect(t).not.toContain('忙');
    expect(t).not.toContain('空闲');
  });
});

describe('runHint（磁贴上的"会落到哪"提示）', () => {
  function withBusy(map: Record<number, Partial<BusyEntry>>, ids = [1, 2]) {
    const term = useTerminalStore();
    term.sessions = ids.map(termRow);
    api = fakeApi([row()], { busy: map });
    setApi(api.api as never, location as never);
    return useQuickCmdStore();
  }

  // 提示不能假装知道命令会进哪个会话：挑目标是后端的规则
  // （最少被触碰的那个），前端复制一份迟早和它分家，届时每次提示都在骗人。
  it('有空闲会话时说"直接执行"，不点名下哪个会话', async () => {
    const s = withBusy({ 1: { busy: true, foreground: 'apt' }, 2: { busy: false } });
    await s.refreshBusy();
    expect(s.runHint).toContain('直接执行');
    expect(s.runHint).not.toContain('lp-1');
    expect(s.runHint).not.toContain('lp-2');
  });

  it('一个会话都没有时说"会新建会话"，不说"直接执行"', () => {
    const s = withBusy({}, []);
    expect(s.runHint).toContain('新建');
    expect(s.runHint).not.toContain('直接执行');
  });

  it('全会忙时说"会新建会话"（D20）', async () => {
    const s = withBusy({ 1: { busy: true, foreground: 'yes' }, 2: { busy: true, foreground: 'apt' } });
    await s.refreshBusy();
    expect(s.runHint).toContain('新建');
    expect(s.runHint).not.toContain('直接执行');
  });

  it('忙闲查不到时不承诺任何事', async () => {
    const s = withBusy({}); // 后端一个都没答
    await s.refreshBusy();
    expect(s.runHint).not.toContain('直接执行');
    expect(s.runHint).not.toContain('新建');
  });
});

// 终端标签上的"· 忙"（原型 lp-2 · 忙）。标签宽度只有那么多，
// 文案必须短；且"不知道"时必须返回空串 —— 标签上挂个"状态未知"
// 会比不挂更让人以为出了故障。
describe('tabBadge（终端标签的忙闲角标）', () => {
  it('忙 → "忙"，空闲或不知道 → 空串', () => {
    expect(tabBadge(busy({ busy: true, foreground: 'apt' }))).toBe('忙');
    expect(tabBadge(busy({ busy: false }))).toBe('');
    expect(tabBadge(undefined)).toBe('');
  });
});
