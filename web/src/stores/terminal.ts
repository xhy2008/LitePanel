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
  // 异常/消失标签的"遗言"：最后一次输出（capture-pane 在尸体上读得到）。
  // 点尸体标签时经 loadOutput 拉取；空串=没有可读历史或还没拉。
  output: string;
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
    output: '',
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
        // 标签栏的准入规则：活着，或者**带着死因**死去。
        //
        // 死因（exit_status）决定一切：>0 = 命令异常退出、-1 = 会话凭空
        // 消失，这两种都要留在标签栏让用户看见并手动删除（真机反馈的
        // 反面是"灰色标签挂一堆"，但那次的病根其实是分不清哪个是异常）。
        // 正常退出（0）由后端自动删行，这里露出来也只可能是残留；
        // null 的死行来自死因机制上线之前，同样不翻出来打扰。
        const shown = (r.sessions ?? []).filter((x) => shouldShow(x));
        // "刚刚还在"由跟上一次列表做差来保证，不需要额外的 loaded 标志位：
        // 首次载入时 this.sessions 本来就是空表，差集自然是空 —— 库里那些
        // 几小时前退出的历史行不会触发提示。（曾经加过 `if (this.loaded)`，
        // 变异测试证明它是死代码：删掉它任何测试都不红。）
        // 两种"刚刚还在，现在没了"都要提示，来源不同：
        //   变化在列表里（带死因的死行留下）→ 提示里报退出码；
        //   整行从列表里消失（正常退出被后端自动删）→ 只报标题。
        // 少了后者，用户看到的就是标签凭空蒸发，什么解释都没有。
        // 每轮重算而不是"只在有人死时设置"：同名会话复活之后，上一轮
        // 那句"已退出"就过期了，留着是谎报。
        const died = this.sessions.filter((p) => p.alive && !all.some((x) => x.id === p.id && x.alive));
        if (died.length) {
          const label = (g: TermSessionRow) => {
            const fresh = all.find((x) => x.id === g.id);
            return fresh ? deathLabel(g, fresh) : g.title;
          };
          this.notice = `会话已退出：${died.map(label).join('、')}`;
        } else {
          this.notice = '';
        }
        this.sessions = shown;
        // active 只在它**从标签栏消失**时才挪走。异常退出不动 active：
        // 用户就看着它死在眼前，跳到别的标签等于把案发现场撤了。
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

    /** 拉一个会话的最后输出（遗言）。见 state.output 的注释。 */
    async loadOutput(id: number) {
      // 先清空再发请求：上一个会话的遗言绝不能在这一帧盖在新的上面
      // （甲死因是 137、乙根本没有历史 —— 中间帧显示甲的字就是编故事）。
      this.output = '';
      const { api } = getApi();
      try {
        const r = await api.get<{ output: string }>(`/api/term/sessions/${id}/output`);
        this.output = r.output ?? '';
      } catch {
        // 不弹红色错误条：遗言是"能看见最好"的补充信息，拉不到就显示
        // "无输出记录"。真正要用户处理的事（删除/重开）浮层里都有。
        this.output = '';
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

/** 死行要不要出现在标签栏：活着，或带着可展示的死因。 */
function shouldShow(x: TermSessionRow): boolean {
  return x.alive || x.exit_status === -1 || (x.exit_status ?? 0) > 0;
}

/** "跑任务（退出码 137）"；-1 没有码可报，说"已消失"。 */
function deathLabel(old: TermSessionRow, fresh: TermSessionRow): string {
  const code = fresh.exit_status;
  if (code === null) return old.title;
  return code === -1 ? `${old.title}（已消失）` : `${old.title}（退出码 ${code}）`;
}

/** 死因角标文案（与 quickcmd 的忙闲 tabBadge 是两种角标：那个说忙/空闲，这个说死亡原因）。：活着没有角标；异常显示退出码；消失没有码可报。 */
export function deathBadge(x: TermSessionRow): string {
  if (x.alive) return '';
  if (x.exit_status === -1) return '消失';
  if (x.exit_status !== null && x.exit_status > 0) return `异常(${x.exit_status})`;
  return '';
}
