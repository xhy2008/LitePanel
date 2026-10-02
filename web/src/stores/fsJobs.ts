import { defineStore } from 'pinia';
import { getApi } from '../api/inject';
import { isActive } from '../api/fsJobs';
import type { JobInput, JobProgress, JobRow } from '../api/fsJobs';

interface State {
  items: JobRow[];
  loaded: boolean;
  loading: boolean;
  error: string;
}

/**
 * 后台文件任务列表（右下角任务抽屉的数据源）。
 *
 * 状态以 WS fsjobs 推送为准，HTTP 只在首次与兜底时拉：任务进度的权威
 * 来源是执行侧（每 200ms/4MB 落一次库），前端再自己造一套轮询只会让
 * 抽屉里的数字比后端还滞后。
 */
export const useFsJobsStore = defineStore('fsJobs', {
  state: (): State => ({ items: [], loaded: false, loading: false, error: '' }),

  getters: {
    /** 有没有还在跑/排队的任务：抽屉用它决定要不要保持连接与角标。 */
    hasActive(s): boolean {
      return s.items.some((j) => isActive(j.state));
    },
    activeCount(s): number {
      return s.items.filter((j) => isActive(j.state)).length;
    },
  },

  actions: {
    async load() {
      this.loading = true;
      try {
        // getApi() 本身会抛（api 未注入）：把它放在 try 里面，load 的
        // 契约就是"永不 reject、失败进 error"，调用方不必再包一层。
        const { api } = getApi();
        const r = await api.get<{ jobs: JobRow[] | null }>('/api/fs/jobs');
        // 后端空列表回 []；但 null 会让视图里的 .map 整块炸成白屏。
        this.items = r.jobs ?? [];
        this.loaded = true;
        this.error = '';
      } catch (e) {
        this.error = messageOf(e);
      } finally {
        this.loading = false;
      }
    },

    /**
     * 合并一次 WS 推送。
     *
     * 认不出来的 id 必须**重拉而不是就地插入**：推送是整行的有界子集
     * （不含 src），插进列表会让任何读 `j.src.length` 的视图拿到
     * undefined —— 那种崩溃只在"别的标签页刚提交了一个任务"时出现，
     * 平时根本碰不到。
     */
    applyProgress(p: JobProgress) {
      const idx = this.items.findIndex((i) => i.id === p.id);
      if (idx < 0) {
        void this.load();
        return;
      }
      const before = this.items[idx];
      this.items[idx] = {
        ...before,
        op: p.op,
        dst: p.dst,
        total_bytes: p.total_bytes,
        done_bytes: p.done_bytes,
        entries_total: p.entries_total,
        entries_done: p.entries_done,
        state: p.state,
        cancel_requested: p.cancel_requested,
        permanent: p.permanent,
        resumed: p.resumed,
        error: p.error ?? '',
        updated_at: p.updated_at,
      };
    },

    /** 提交任务。返回 job_id，调用方（文件页）靠它立刻在抽屉里定位这条。 */
    async submit(input: JobInput): Promise<number> {
      const { api } = getApi();
      const r = await api.post<{ job_id: number } & JobRow>('/api/fs/jobs', input);
      // 本地先并进列表，不等下一次 load：提交后抽屉要立刻出现这一条，
      // 否则用户点了"删除"看到空抽屉，第一反应是没生效、会再点一次。
      this.upsert(r);
      this.error = '';
      return r.job_id;
    },

    /**
     * 请求取消。
     *
     * 成功后不写 state，只写 cancel_requested：终态是后端（拥有任务的
     * 那一方）的事，前端抢着写"已取消"会骗用户说文件已经不动了，而
     * worker 可能还差几个文件。界面上"正在取消…"由 cancel_requested 渲染。
     */
    async cancel(id: number) {
      if (this.items.every((j) => j.id !== id)) return;
      const { api } = getApi();
      try {
        await api.del(`/api/fs/jobs/${id}`);
        this.patch(id, { cancel_requested: true });
        this.error = '';
      } catch (e) {
        this.error = messageOf(e);
        throw e;
      }
    },

    /** 重试一条 interrupted。成功后后端会推 pending，这里只顺手把行更新掉。 */
    async retry(id: number) {
      const { api } = getApi();
      try {
        const r = await api.post<JobRow & { job_id: number }>(`/api/fs/jobs/${id}/retry`);
        this.upsert(r);
        this.error = '';
      } catch (e) {
        this.error = messageOf(e);
        throw e;
      }
    },

    upsert(row: JobRow) {
      const idx = this.items.findIndex((i) => i.id === row.id);
      if (idx < 0) {
        this.items = [row, ...this.items];
        return;
      }
      this.items[idx] = { ...this.items[idx], ...row };
    },

    patch(id: number, over: Partial<JobRow>) {
      const idx = this.items.findIndex((i) => i.id === id);
      if (idx < 0) return;
      this.items[idx] = { ...this.items[idx], ...over };
    },
  },
});

function messageOf(e: unknown): string {
  const m = (e as { message?: string })?.message;
  return m || '操作失败';
}
