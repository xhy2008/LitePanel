import { defineStore } from 'pinia';
import {
  addTask,
  clearHistory,
  fetchHealth,
  fetchSummary,
  fetchTasks,
  pauseTask,
  removeTask,
  resumeTask,
  type DlAddInput,
  type DlEvent,
  type DlHealth,
  type DlSummary,
  type DlTask,
} from '../api/downloads';

function messageOf(e: unknown): string {
  const m = (e as { message?: string })?.message;
  return m && m.trim() ? m : '操作失败';
}

export const DL_POLL_MS = 3000;

interface State {
  tasks: DlTask[];
  summary: DlSummary | null;
  health: DlHealth | null;
  loaded: boolean;
  loading: boolean;
  /** 列表级错误（拉取失败）。任务级错误在 task.error 里，别混。 */
  error: string;
  /** 最近一次操作（暂停/删除/添加…）的失败原因，显示在对话框或行内。 */
  actionError: string;
  busy: boolean;
}

// 事件到达后的合并刷新延迟：BT/多源任务的状态变化可能成串到达，每个事件都
// 全量重拉会打成风暴。合并到一个短窗口里只拉一次。
const REFRESH_DEBOUNCE_MS = 300;
let refreshTimer: ReturnType<typeof setTimeout> | null = null;

export const useDownloadsStore = defineStore('downloads', {
  state: (): State => ({
    tasks: [],
    summary: null,
    health: null,
    loaded: false,
    loading: false,
    error: '',
    actionError: '',
    busy: false,
  }),

  getters: {
    // 四组（IDM 风格）：下载中（active+waiting 同组，waiting 只是没轮到）、
    // 已暂停、已完成、失败。按 state 在前端分组而不是让后端出接口参数：
    // 后端 /dl/tasks 本来就全量返回，加过滤参数是第二套取数逻辑。
    active: (s) => s.tasks.filter((t) => t.state === 'active' || t.state === 'waiting'),
    paused: (s) => s.tasks.filter((t) => t.state === 'paused'),
    done: (s) => s.tasks.filter((t) => t.state === 'complete'),
    failed: (s) => s.tasks.filter((t) => t.state === 'error'),
  },

  actions: {
    async load() {
      this.loading = true;
      this.error = '';
      try {
        // health 失败不拖垮列表：aria2 掉了也要能看到历史任务（它们躺在面板
        // 自己的库里）。三个请求并行且各自兜底，任何一个坏都不该白屏。
        const [tasks, summary, health] = await Promise.all([
          fetchTasks(),
          fetchSummary().catch(() => null),
          fetchHealth().catch(() => null),
        ]);
        this.tasks = tasks;
        this.summary = summary;
        this.health = health;
        this.loaded = true;
      } catch (e) {
        this.error = messageOf(e);
      } finally {
        this.loading = false;
      }
    },

    /** WS 事件入口：事件只带 gid 不带状态，所以收到就去重刷一次。 */
    applyEvent(ev: DlEvent) {
      void this.scheduleRefresh();
      void ev;
    },

    scheduleRefresh(): Promise<void> {
      // 合并窗口：连点三个暂停、或 BT 任务连发状态事件，都只触发一次重拉。
      if (refreshTimer) clearTimeout(refreshTimer);
      return new Promise((resolve) => {
        refreshTimer = setTimeout(async () => {
          refreshTimer = null;
          try {
            this.tasks = await fetchTasks();
          } catch {
            /* 轮询还会兜住，这里不打断用户 */
          }
          resolve();
        }, REFRESH_DEBOUNCE_MS);
      });
    },

    async add(input: DlAddInput): Promise<boolean> {
      this.actionError = '';
      this.busy = true;
      try {
        await addTask(input);
        await this.scheduleRefresh();
        return true;
      } catch (e) {
        this.actionError = messageOf(e);
        return false;
      } finally {
        this.busy = false;
      }
    },

    async pause(gid: string) {
      await this.control(() => pauseTask(gid));
    },

    async resume(gid: string) {
      // 失败任务的重试也走 resume：aria2 对 error 状态的任务 resume 即重下。
      await this.control(() => resumeTask(gid));
    },

    async remove(gid: string, force: boolean) {
      await this.control(() => removeTask(gid, force));
    },

    async clearHist(): Promise<boolean> {
      this.actionError = '';
      this.busy = true;
      try {
        await clearHistory();
        await this.scheduleRefresh();
        return true;
      } catch (e) {
        this.actionError = messageOf(e);
        return false;
      } finally {
        this.busy = false;
      }
    },

    /** 控制类动作的公共壳：失败原因必须可见，成功后合并刷新。 */
    async control(fn: () => Promise<void>) {
      this.actionError = '';
      try {
        await fn();
        await this.scheduleRefresh();
      } catch (e) {
        this.actionError = messageOf(e);
      }
    },
  },
});
