import { defineStore } from 'pinia';
import { getApi } from '../api/inject';
import { useTerminalStore } from './terminal';
import type { BusyEntry, CommandInput, CommandRow, RunResult } from '../api/quickcmd';

function messageOf(e: unknown): string {
  const m = (e as { message?: string })?.message;
  return m && m.trim() ? m : '操作失败';
}

function codeOf(e: unknown): string {
  return (e as { code?: string })?.code ?? '';
}

/**
 * 会话忙闲的提示文案。唯一来源，视图不许自己拼：两处写法一漂移，同一个
 * 状态在磁贴和 toast 上就是两句话。
 *
 * 传 undefined 表示"不知道"（后端没答这个会话）。这时候既不能说"空闲"
 * —— 那是骗人；也不能说"忙" —— 那会把能点的按钮看着像不能点。
 */
export function describeRun(state?: BusyEntry): string {
  if (!state) return '状态未知';
  if (!state.busy) return '空闲，直接执行';
  // "在跑什么"是用户决定等不等的全部依据；前台就是 shell 本身时不算什么
  // 值得一提的进程，摆出来反而像噪声。
  const what = state.foreground && state.foreground !== state.shell_name ? ` · ${state.foreground}` : '';
  return `忙${what}`;
}

interface State {
  items: CommandRow[];
  loaded: boolean;
  loading: boolean;
  error: string;
  // 一次性提示（toast 消费后 clearNotice）。执行结果必须走这里而不是只
  // return 给调用方：磁贴点完就跳页，组件在跳转中会被卸载，
  // return 的值没有任何人会读。
  notice: string;
  // 会话 id → 忙闲。键缺失 = 不知道（见 describeRun）。
  busy: Record<number, BusyEntry>;
  // 每条命令一个"在飞"标记：重复点会真的往终端里灌两遍命令，
  // 而对 rm -rf 这类命令，第二遍是事故不是重复。
  pending: Record<number, boolean>;
}

export const useQuickCmdStore = defineStore('quickcmd', {
  state: (): State => ({ items: [], loaded: false, loading: false, error: '', notice: '', busy: {}, pending: {} }),

  actions: {
    clearNotice() {
      this.notice = '';
    },

    async load() {
      const { api } = getApi();
      this.loading = true;
      try {
        const r = await api.get<{ commands: CommandRow[] | null }>('/api/commands');
        // 后端空列表给 []；但 null 会让视图的 v-for 整块炸掉。
        this.items = r.commands ?? [];
        this.loaded = true;
        this.error = '';
      } catch (e) {
        this.error = messageOf(e);
      } finally {
        this.loading = false;
      }
    },

    async create(input: CommandInput): Promise<void> {
      const { api } = getApi();
      await api.post<CommandRow>('/api/commands', input);
      this.error = '';
      await this.load();
    },

    async update(id: number, input: Partial<CommandInput>): Promise<void> {
      const { api } = getApi();
      await api.patch<CommandRow>(`/api/commands/${id}`, input);
      this.error = '';
      await this.load();
    },

    async remove(id: number): Promise<void> {
      const { api } = getApi();
      await api.del(`/api/commands/${id}`);
      await this.load();
    },

    async move(id: number, direction: 'up' | 'down'): Promise<void> {
      const { api } = getApi();
      try {
        await api.post(`/api/commands/${id}/move`, { direction });
        this.error = '';
      } catch (e) {
        this.error = messageOf(e);
        throw e;
      }
      await this.load();
    },

    /**
     * 一次问一批会话的忙闲。
     *
     * 必须批量：前端有四个标签，逐个请求就是四倍往返，而每次往返在 tmux
     * 上都是一次 fork。会话清单直接读终端 store，不在这里另存一份 ——
     * 两份会话列表一定会漂移到"忙角标标在已经不存在的标签上"。
     */
    async refreshBusy(): Promise<void> {
      const term = useTerminalStore();
      const ids = term.sessions.map((s) => s.id);
      if (ids.length === 0) {
        this.busy = {};
        return;
      }
      const { api } = getApi();
      const q = ids.map((id) => `session=${id}`).join('&');
      try {
        const r = await api.get<{ sessions: BusyEntry[] | null }>(`/api/commands/busy?${q}`);
        const next: Record<number, BusyEntry> = {};
        for (const s of r.sessions ?? []) next[s.session_id] = s;
        this.busy = next;
      } catch {
        // 清成"全不知道"而不是"全空闲"：后者会诱导用户往正在跑东西的
        // 会话里投命令。真原因可能是 tmux 刚被关掉。
        this.busy = {};
      }
    },

    /**
     * 执行快捷命令（D20）：注入 tmux，然后跳终端页并选中那个标签。
     *
     * 返回 false 表示"被去重挡掉了"（不是失败）；失败一律 throw，让调用方
     * 能区分"用户连点"和"真出错了"。
     */
    async run(
      cmd: CommandRow,
      opts: { confirm?: boolean; goTerm?: () => void } = {},
    ): Promise<boolean> {
      if (this.pending[cmd.id]) return false;
      this.pending = { ...this.pending, [cmd.id]: true };
      const { api } = getApi();
      const needConfirm = opts.confirm || cmd.need_confirm;
      const path = `/api/commands/${cmd.id}/run${needConfirm ? '?confirm=1' : ''}`;
      try {
        const res = await api.post<RunResult>(path);
        await this.landOnSession(res, opts.goTerm);
        // 新建了会话必须说：命令出现在一个用户没点过的标签里，
        // 不说就是"屏幕上凭空多出一行字"。
        this.notice = res.is_new_session
          ? `当前会话都在忙，已新建会话 ${res.title} 执行`
          : '';
        this.error = '';
        return true;
      } catch (e) {
        // 按后端的 code 说话，不许统一成"操作失败"：
        // 那会把"这条命令要你确认"读成"面板坏了"。
        const code = codeOf(e);
        if (code === 'session_busy') {
          this.notice = '终端会话都在忙，稍后再试';
          // 后端刚亲口说会话忙 —— 本地那份"空闲"图样已被证伪，必须重取。
          // 不重取的话磁贴继续挂着"空闲"，用户会照着它一直点、一直失败，
          // 而且每次都失败在同一句提示上。忙闲的唯一来源是后端，
          // 这里只是它的缓存，被证伪就得回填。
          await this.refreshBusy();
        }
        else if (code === 'confirm_required') this.notice = '该命令需要确认后才会执行';
        else this.notice = messageOf(e);
        throw e;
      } finally {
        const rest = { ...this.pending };
        delete rest[cmd.id];
        this.pending = rest;
      }
    },

    /**
     * 把终端页指到注入进去的那个会话上。
     *
     * "跳页"和"选中标签"是两件事，少一件都是 bug：只选不跳，用户停在命令
     * 页以为没反应；只跳不选，他盯着另一个会话等那行字出现。
     */
    async landOnSession(res: RunResult, goTerm?: () => void) {
      const term = useTerminalStore();
      if (!term.sessions.some((s) => s.id === res.session_id)) {
        // 面板刚新建过会话而本地列表还没刷新时会发生。不重拉就选不上，
        // 用户看到的是"命令执行了，可屏幕上什么都没多"。
        await term.load();
      }
      term.select(res.session_id);
      // goTerm 由调用方注入（组件里是 router.push）。store 不直接 import
      // router：那样每个 store 测试都要拖一个路由进来，而路由本身不是这里
      // 要证明的东西。没传时不静默跳过 —— 那正是"执行了却看不到"的形态。
      if (!goTerm) {
        throw new Error('run 必须提供 goTerm：否则命令注入到了一个没人选中的会话');
      }
      goTerm();
    },
  },
});
