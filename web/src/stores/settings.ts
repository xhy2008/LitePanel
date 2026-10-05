import { defineStore } from 'pinia';
import { getApi } from '../api/inject';
import type { SettingItem, SettingsResponse, SettingsSaveResponse } from '../api/settings';

function messageOf(e: unknown): string {
  const m = (e as { message?: string })?.message;
  return m && m.trim() ? m : '操作失败';
}

interface State {
  items: SettingItem[];
  loaded: boolean;
  loading: boolean;
  saving: boolean;
  error: string;
  // 保存成功的一次性提示（含"部分项未能立即生效"这种要如实转达的话）。
  notice: string;
  // key -> 用户改过但还没保存的值。只在有改动时出现；保存成功后清空。
  // 与 items 分开存：items 是"服务器上的真相"，dirty 是"本地草稿"，两者混在
  // 一处就没法回答"到底改了哪几项"（而这决定 PUT 发什么）。
  dirty: Record<string, number | string>;
}

export const useSettingsStore = defineStore('settings', {
  state: (): State => ({
    items: [],
    loaded: false,
    loading: false,
    saving: false,
    error: '',
    notice: '',
    dirty: {},
  }),

  getters: {
    // 当前应显示的值：本地草稿优先，否则服务器值。控件绑这个。
    display: (s) => (key: string): number | string =>
      key in s.dirty ? s.dirty[key] : (s.items.find((i) => i.key === key)?.value ?? ''),

    dirtyCount: (s) => Object.keys(s.dirty).length,

    // 只有改了、且不含"只改了密钥留空"这类无效项时才允许保存。
    canSave: (s) => Object.keys(s.dirty).length > 0 && !s.saving,

    groups: (s) => {
      const order = ['dashboard', 'auth', 'files', 'services', 'terminal', 'download'];
      const byGroup = new Map<string, SettingItem[]>();
      for (const it of s.items) {
        const arr = byGroup.get(it.group) ?? [];
        arr.push(it);
        byGroup.set(it.group, arr);
      }
      // 已知分组按固定顺序，未知分组追加在后（新分组不会因为漏登记而消失）。
      const keys = [...byGroup.keys()].sort((a, b) => {
        const ia = order.indexOf(a);
        const ib = order.indexOf(b);
        return (ia < 0 ? 999 : ia) - (ib < 0 ? 999 : ib);
      });
      return keys.map((g) => ({ group: g, items: byGroup.get(g)! }));
    },
  },

  actions: {
    async load() {
      this.loading = true;
      this.error = '';
      try {
        const { api } = getApi();
        const r = await api.get<SettingsResponse>('/api/settings');
        this.items = r.groups.flatMap((g) => g.items);
        this.loaded = true;
        this.dirty = {};
      } catch (e) {
        this.error = messageOf(e);
      } finally {
        this.loading = false;
      }
    },

    edit(key: string, value: number | string) {
      const item = this.items.find((i) => i.key === key);
      // 与服务器值相同就撤掉草稿标记：否则用户点进输入框又原样退出，
      // "有 N 项未保存"会一直挂着，保存按钮灰不掉。
      if (item && value === item.value) {
        delete this.dirty[key];
        return;
      }
      this.dirty[key] = value;
    },

    revert(key: string) {
      delete this.dirty[key];
    },

    clearNotice() {
      this.notice = '';
    },

    async save(): Promise<boolean> {
      if (!this.canSave) return false;
      this.saving = true;
      this.error = '';
      try {
        const { api } = getApi();
        // 只发改过的项：全量提交一旦有哪项没渲染出来（比如密钥项被前端隐藏），
        // 就会把它的值悄悄清掉。
        const body: Record<string, number | string> = { ...this.dirty };
        const r = await api.put<SettingsSaveResponse>('/api/settings', body);
        this.items = r.groups.flatMap((g) => g.items);
        this.dirty = {};
        // 后端对"值已写库但某项没能立即生效"回 applied:false + reason。这句
        // 必须原样转达：设置项静默不生效是本模块明令禁止的失效模式。
        //
        // 需重启的提醒必须在两个分支都附加：aria2 连接类项是"只重启生效",
        // applier 根本不推它，保存回的是 applied:true —— 若只在 false 分支
        // 提重启，用户改了地址看到"已保存并生效"，不重启就以为改动丢了。
        const hint = restartHint(this.items, body);
        this.notice = r.applied
          ? `已保存并生效${hint}`
          : `已保存，但有项未能立即生效：${r.reason || '原因未知'}${hint}`;
        return true;
      } catch (e) {
        this.error = messageOf(e);
        return false;
      } finally {
        this.saving = false;
      }
    },

    async revokeAllSessions(): Promise<boolean> {
      this.error = '';
      try {
        const { api } = getApi();
        await api.post('/api/sessions/revoke-all');
        // 后端把当前会话也吊销并清了 cookie；交给路由守卫跳登录。
        return true;
      } catch (e) {
        this.error = messageOf(e);
        return false;
      }
    },
  },
});

// 本次提交里含"需重启"项时，在提示里补一句 —— 否则用户以为改了就好，
// 而重启前什么都是旧的。
function restartHint(items: SettingItem[], changed: Record<string, unknown>): string {
  const hit = items.some((i) => i.restart_required && i.key in changed);
  return hit ? '（其中含需重启才生效的项）' : '';
}
