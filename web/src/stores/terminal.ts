import { defineStore } from 'pinia';
import { getApi } from '../api/inject';
import type { TermSessionInput, TermSessionRow } from '../api/terminal';

/**
 * 自动标题：无用户输入时取下一个可用序号。
 *
 * 单独导出成纯函数是为了能表驱动地测"占用"规则。撞名不会报错，只会
 * 让标签栏出现两个"会话 2" —— 用户分不清哪个在跑任务，于是把不该关的
 * 关掉。所以这里必须跳过已占用的编号，而不是 `count + 1`。
 *
 * 只统计自动生成的形状（会话 + 纯数字）：用户自取的"会话 旧机器迁移"
 * 也带"会话"二字，把它算进编号就会从莫名其妙的地方开始编。
 */
export function autoTitle(wanted: string, existing: { title: string }[]): string {
  const t = wanted.trim();
  if (t) return t;
  const taken = new Set<number>();
  for (const e of existing) {
    const m = /^会话 (\d+)$/.exec(e.title ?? '');
    if (m) taken.add(Number(m[1]));
  }
  for (let n = 1; ; n += 1) {
    if (!taken.has(n)) return `会话 ${n}`;
  }
}

/** 后端文案优先；没有兜底文案的话界面上会出现一个空白错误条。 */
export function errorMessage(e: unknown): string {
  const m = (e as { message?: string } | null)?.message;
  return m && m.trim() ? m : '操作失败，请重试';
}

interface State {
  sessions: TermSessionRow[];
  activeId: number;
  loaded: boolean;
  loading: boolean;
  creating: boolean;
  error: string;
  // 没装 tmux 时后端返回 501。必须与"你还没有会话"区分开：
  // 后者会让用户一直去找那个根本不存在的新建按钮。
  unavailable: boolean;
  // "会话 X 已退出"这类一次性提示，由壳层（App.vue）摆出来。
  // 用 state 而不是 return 值：会话是在 load() 里消失的，而 load 的调用方
  // 常常是轮询 —— 没人会去读一个后台轮询的返回值。
  notice: string;
}

export const useTerminalStore = defineStore('terminal', {
  state: (): State => ({
    sessions: [],
    activeId: 0,
    loaded: false,
    loading: false,
    creating: false,
    error: '',
    unavailable: false,
    notice: '',
  }),

  getters: {
    active(s): TermSessionRow | undefined {
      return s.sessions.find((x) => x.id === s.activeId);
    },
  },

  actions: {
    async load() {
      const { api } = getApi();
      this.loading = true;
      try {
        const r = await api.get<{ sessions: TermSessionRow[] | null }>('/api/term/sessions');
        const all = r.sessions ?? [];
        // 退出的会话不进标签栏（真机反馈：它们变成灰色标签挂在那儿，
        // 用户要点进去才知道没了）。后端仍然保留 alive=0 的行 —— 那里是
        // "这个会话曾经存在"的唯一记录，也是本条提示的依据。
        const alive = all.filter((x) => x.alive);
        // "刚刚还在"由跟上一次列表做差来保证，不需要额外的 loaded 标志位：
        // 首次载入时 this.sessions 本来就是空表，差集自然是空 —— 库里那些
        // 几小时前退出的历史行不会触发提示。（曾经加过 `if (this.loaded)`，
        // 变异测试证明它是死代码：删掉它任何测试都不红。）
        const gone = this.sessions.filter(
          (p) => p.alive && !alive.some((x) => x.id === p.id),
        );
        if (gone.length) {
          this.notice = `会话已退出：${gone.map((g) => g.title).join('、')}`;
        }
        this.sessions = alive;
        // 只在没有选中（或选中项已不存在）时落到第一个：会话切换是有状态的
        // （那个 shell 正在跑东西），不能被一次刷新抢走。
        if (!this.sessions.some((s) => s.id === this.activeId)) {
          this.activeId = this.sessions[0]?.id ?? 0;
        }
        this.loaded = true;
        this.unavailable = false;
        this.error = '';
      } catch (e) {
        this.unavailable = (e as { status?: number }).status === 501;
        this.error = errorMessage(e);
      } finally {
        this.loading = false;
      }
    },

    /** 显式重拉（刷新按钮 / 标签丢失后自愈）。语义与 load 一致。 */
    async reload() {
      await this.load();
    },

    async create(input: TermSessionInput): Promise<number> {
      const { api } = getApi();
      this.creating = true;
      try {
        const title = autoTitle(input.title, this.sessions);
        const r = await api.post<{ session: TermSessionRow }>('/api/term/sessions', {
          title,
          ...(input.cwd ? { cwd: input.cwd } : {}),
          ...(input.shell ? { shell: input.shell } : {}),
          ...(input.history_limit ? { history_limit: input.history_limit } : {}),
        });
        this.sessions = [...this.sessions, r.session];
        this.activeId = r.session.id;
        this.error = '';
        return r.session.id;
      } catch (e) {
        this.error = errorMessage(e);
        throw e;
      } finally {
        this.creating = false;
      }
    },

    async rename(id: number, title: string) {
      const idx = this.sessions.findIndex((s) => s.id === id);
      if (idx < 0) return;
      const before = this.sessions[idx];
      const { api } = getApi();
      try {
        const r = await api.patch<{ session: TermSessionRow }>(`/api/term/sessions/${id}`, {
          title: title.trim(),
        });
        this.sessions[idx] = r.session;
        this.error = '';
      } catch (e) {
        // 失败时不改标题（也就不需要回滚）：留着新标题等于界面在骗人，
        // 下次重连它又变回旧的，用户会以为自己记错了。
        this.sessions[idx] = before;
        this.error = errorMessage(e);
        throw e;
      }
    },

    async remove(id: number) {
      const { api } = getApi();
      try {
        // confirm=1 由前端带上、后端强制：只在前端弹确认框的话，
        // 脚本或漏改的调用会把正在跑任务的会话连 shell 一起杀掉。
        await api.del(`/api/term/sessions/${id}?confirm=1`);
      } catch (e) {
        // 删失败绝不能把行摘掉：那会变成"面板以为删了、tmux 里还在跑"，
        // 而且用户再也点不到它。
        this.error = errorMessage(e);
        throw e;
      }
      const rest = this.sessions.filter((s) => s.id !== id);
      this.sessions = rest;
      if (this.activeId === id) this.activeId = rest[0]?.id ?? 0;
      this.error = '';
    },

    clearNotice() {
      this.notice = '';
    },

    select(id: number) {
      if (this.activeId !== id && this.sessions.some((s) => s.id === id)) this.activeId = id;
    },
  },
});
