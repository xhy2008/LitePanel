import { defineStore } from 'pinia';
import { getApi } from '../api/inject';
import type { ServiceEvent, ServiceInput, ServiceRow, ServiceState } from '../api/services';

/**
 * D21：退出呈现只有三种 —— 运行中 / 正常退出 code 0 / 异常退出 code N。
 * 这里就是那段文案的唯一来源，视图不许自己拼。
 */
export interface ServiceDesc {
  tone: 'run' | 'good' | 'bad' | 'idle';
  text: string;
  badge: string;
  icon: string;
}

function pad(n: number): string {
  return n < 10 ? `0${n}` : String(n);
}

/** 退出时刻：当天只给时分，昨天说"昨天"，更早给日期。 */
export function whenLabel(epochSec: number, now = Date.now()): string {
  if (!epochSec) return '';
  const d = new Date(epochSec * 1000);
  const today = new Date(now);
  const sameDay = d.toDateString() === today.toDateString();
  if (sameDay) return `${pad(d.getHours())}:${pad(d.getMinutes())}`;
  const yest = new Date(now - 86400_000);
  if (d.toDateString() === yest.toDateString()) return '昨天';
  return `${d.getMonth() + 1}/${d.getDate()}`;
}

/** 运行时长：原型写的是"2 小时"这种口语量级，不写秒。 */
export function uptimeLabel(startedAt: number, now = Date.now()): string {
  if (!startedAt) return '';
  const s = Math.max(0, Math.floor(now / 1000 - startedAt));
  if (s < 60) return `${s} 秒`;
  if (s < 3600) return `${Math.floor(s / 60)} 分钟`;
  if (s < 86400) return `${Math.floor(s / 3600)} 小时`;
  return `${Math.floor(s / 86400)} 天`;
}

export function describeService(r: ServiceRow, now = Date.now()): ServiceDesc {
  const badge = r.kind === 'systemd' ? 'SYSV' : 'CMD';
  switch (r.state) {
    case 'running':
      return {
        tone: 'run',
        badge,
        icon: 'robot',
        text: `PID ${r.pid}${uptimeLabel(r.started_at, now) ? ' · ' + uptimeLabel(r.started_at, now) : ''}`,
      };
    case 'starting':
      return { tone: 'run', badge, icon: 'robot', text: '启动中…' };
    case 'stopping':
      return { tone: 'run', badge, icon: 'robot', text: '停止中…' };
  }

  // stopped。exit_reason 缺席意味着从没退出过 —— 绝不能显示 "code 0"，
  // 那是"跑过且好好退出"的意思，会把用户带去完全错误的排查方向。
  if (!r.exit_reason) {
    return { tone: 'idle', badge, icon: 'stop', text: '未启动' };
  }
  const at = whenLabel(r.exit_at ?? 0, now);
  const tail = at ? ` · ${at}` : '';
  // 面板自己停的（含面板重启时的连带关停）：退出码固定是 143，即我们
  // 自己发的 SIGTERM。按 D21 归 clean 正确，但把 143 直接摆出来会被
  // 读成故障。归因说清楚，仍然只占 clean/error 两态。
  if (r.exit_reason === 'clean' && (r.stopped_by === 'user' || r.stopped_by === 'panel-shutdown')) {
    const why = r.stopped_by === 'user' ? '已停止' : '已停止 · 面板关停';
    return { tone: 'good', badge, icon: 'check', text: `${why}${tail}` };
  }
  if (r.exit_reason === 'clean') {
    return { tone: 'good', badge, icon: 'check', text: `正常退出 · code ${r.exit_code ?? 0}${tail}` };
  }
  return { tone: 'bad', badge, icon: 'alert', text: `异常退出 · code ${r.exit_code ?? 0}${tail}` };
}

interface State {
  items: ServiceRow[];
  loaded: boolean;
  loading: boolean;
  error: string;
  // 每个服务一个"请求在飞"标记：连点开关会真的把服务反复启停。
  pending: Record<number, boolean>;
}

export const useServicesStore = defineStore('services', {
  state: (): State => ({ items: [], loaded: false, loading: false, error: '', pending: {} }),

  actions: {
    async load() {
      const { api } = getApi();
      this.loading = true;
      try {
        const r = await api.get<{ services: ServiceRow[] | null }>('/api/services');
        // 后端空列表返回 []；但 null 会让下面的 .map 与视图整块炸掉。
        this.items = r.services ?? [];
        this.loaded = true;
        this.error = '';
      } catch (e) {
        this.error = messageOf(e);
      } finally {
        this.loading = false;
      }
    },

    /**
     * 启停。乐观改状态：等一整趟 HTTP 往返才动，慢机上像"没反应"，
     * 用户就会连点 —— 而连点不是 UI 问题，是会真的把服务反复启停。
     */
    async toggle(id: number): Promise<void> {
      if (this.pending[id]) return;
      const idx = this.items.findIndex((i) => i.id === id);
      if (idx < 0) return;
      const before = { ...this.items[idx] };
      const optimistic: ServiceState =
        before.state === 'running' || before.state === 'starting' ? 'stopping' : 'starting';

      this.items[idx] = { ...before, state: optimistic };
      this.pending = { ...this.pending, [id]: true };
      const { api } = getApi();
      try {
        const after = await api.post<ServiceRow>(`/api/services/${id}/toggle`);
        this.items[idx] = after;
        this.error = '';
      } catch (e) {
        // 必须回滚：留着乐观状态等于骗用户说服务在跑（或已停）。
        const cur = this.items.findIndex((i) => i.id === id);
        if (cur >= 0) this.items[cur] = before;
        this.error = messageOf(e);
        throw e;
      } finally {
        const rest = { ...this.pending };
        delete rest[id];
        this.pending = rest;
      }
    },

    async create(input: ServiceInput): Promise<void> {
      const { api } = getApi();
      await api.post<ServiceRow>('/api/services', input);
      this.error = '';
      await this.load();
    },

    async update(id: number, input: Partial<ServiceInput>): Promise<void> {
      const { api } = getApi();
      await api.patch<ServiceRow>(`/api/services/${id}`, input);
      this.error = '';
      await this.load();
    },

    /** 删除运行中的服务时后端会先停再删，这里只负责刷新列表。 */
    async remove(id: number): Promise<void> {
      const { api } = getApi();
      await api.del(`/api/services/${id}`);
      await this.load();
    },

    /**
     * 合并 WS 事件。就地改而不是重拉列表：重拉会把其他服务在这一瞬间
     * 的实时状态覆盖回旧值，也会让"谁在跑"这个问题多打一枪。
     * 只有事件指向一个不存在的服务（新建/删除）时才重拉。
     */
    applyEvent(ev: ServiceEvent) {
      if (ev.reload) {
        void this.load();
        return;
      }
      const idx = this.items.findIndex((i) => i.id === ev.id);
      if (idx < 0) {
        void this.load();
        return;
      }
      const next = { ...this.items[idx], state: ev.state, pid: ev.pid ?? 0 };
      if (ev.exit) {
        next.exit_reason = ev.exit.reason;
        next.exit_code = ev.exit.code;
        next.exit_signal = ev.exit.signal;
        next.exit_at = ev.exit.at;
        next.stopped_by = ev.exit.stopped_by;
      } else if (ev.state === 'running') {
        // 重新启动：上一次的退出记录要清掉，否则 UI 上会同时出现
        // "运行中"和"上次异常退出 code 137"，读起来自相矛盾。
        delete next.exit_reason;
        delete next.exit_code;
        delete next.exit_signal;
        delete next.exit_at;
        delete next.stopped_by;
      }
      this.items[idx] = next;
    },
  },
});

function messageOf(e: unknown): string {
  const m = (e as { message?: string })?.message;
  return m || '操作失败';
}
