import { defineStore } from 'pinia';
import { getApi } from '../api/inject';
import { joinPath, listQuery } from '../api/files';
import type { FsEntry, FsRoot, ListParams, ListPage, SortKey, TrashItem } from '../api/files';
import { useFsJobsStore } from './fsJobs';

export type ClipMode = 'copy' | 'cut';

export interface Clipboard {
  mode: ClipMode;
  paths: string[];
}

// 每页 500，与设计 8.1 及后端 filemgr.DefaultPageSize 一致。
// 写小一个数会静默改变验收的含义：「10 万文件首屏 ≤500ms」是按每页 500
// 量的，前端偷偷要 200 条就等于换了个更容易达标的问题来问。
const DEFAULT_SIZE = 500;

interface State {
  dir: string;
  entries: FsEntry[];
  page: number;
  total: number;
  size: number;
  sort: SortKey;
  desc: boolean;
  hidden: boolean;
  loading: boolean;
  loadingMore: boolean;
  error: string;
  errorCode: string;
  roots: FsRoot[];
  selected: string[];
  clip: Clipboard | null;
  // 某个盘的回收站建不起来（只读盘 / 根目录不可写）。后端给的是状态码
  // 422 + code=trash_unwritable，这里留个旗标让删除确认框能摆出唯一
  // 可行的出路（永久删除），而不是让用户对着一个必然失败的任务干等。
  trashUnwritable: boolean;
}

export const useFilesStore = defineStore('files', {
  state: (): State => ({
    dir: '',
    entries: [],
    page: 1,
    total: 0,
    size: DEFAULT_SIZE,
    sort: 'name',
    desc: false,
    hidden: false,
    loading: false,
    loadingMore: false,
    error: '',
    errorCode: '',
    roots: [],
    selected: [],
    clip: null,
    trashUnwritable: false,
  }),

  getters: {
    hasMore(s): boolean {
      return s.entries.length < s.total;
    },
    allSelected(s): boolean {
      return s.entries.length > 0 && s.selected.length === s.entries.length;
    },
    canPaste(s): boolean {
      return !!s.clip && s.clip.paths.length > 0;
    },
    selectedPaths(s): string[] {
      // 选中项存的是**文件名**（列表里点选天然知道名字），而任务/剪贴板
      // 要的是绝对路径。这里统一换算，视图就不许自己拼路径（拼错一次
      // 就是"删了另一个目录的同名文件"）。
      return s.selected.map((n) => joinPath(s.dir, n));
    },
  },

  actions: {
    async loadRoots() {
      try {
        const { api } = getApi();
        const r = await api.get<{ roots: FsRoot[] | null }>('/api/fs/roots');
        this.roots = r.roots ?? [];
      } catch (e) {
        this.fail(e);
      }
    },

    /**
     * 打开一个目录（第一页）。
     *
     * 失败时**保留**旧列表、只置 error：后端特意在 entries 为 null 时兜
     * 成 []，就是因为"空列表"会被渲染成"这个目录是空的"。同理，一次
     * 失败的加载如果顺手清空 entries，用户看到的是一目录文件凭空消失,
     * 而他第一反应会是刷新页面 —— 然后按删除。
     */
    async open(path: string) {
      this.loading = true;
      this.error = '';
      this.errorCode = '';
      try {
        const { api } = getApi();
        const q = listQuery({
          path,
          page: 1,
          size: this.size,
          sort: this.sort,
          desc: this.desc,
          hidden: this.hidden,
        });
        const p = await api.get<ListPage>(`/api/fs/list?${q}`);
        // 以**响应回显的 path** 为准，而不是发出去的参数：符号链接会让
        // 两者指向不同目录，而自己拼的 selectedPaths 用的是 dir，
        // 不一致时删除会打到隔壁目录的同名文件上。
        // 回显缺字段（形状漂移 / 拿到了一个错误 JSON）时必须退回请求的
        // 路径：dir 是 undefined 会让面包屑崩掉，而崩在渲染函数里 = 整个
        // 视图白屏且没有错误条，比显示一个旧目录严重得多。
        this.dir = typeof p?.path === 'string' && p.path ? p.path : path;
        this.entries = Array.isArray(p?.entries) ? p.entries : [];
        this.page = p.page;
        this.total = p.total;
        // 换目录必须清空选择：留着上一个目录的选中项，用户在 B 目录
        // 按下删除而实际删掉的是 A 目录的文件 —— 界面说的是假话，
        // 而且是不可逆的那种。
        this.selected = [];
      } catch (e) {
        this.fail(e);
      } finally {
        this.loading = false;
      }
    },

    /** 追加下一页（滚动加载）。失败同样不动已有条目。 */
    async loadMore() {
      if (this.loadingMore || !this.hasMore || !this.dir) return;
      this.loadingMore = true;
      try {
        const { api } = getApi();
        const q = listQuery({
          path: this.dir,
          page: this.page + 1,
          size: this.size,
          sort: this.sort,
          desc: this.desc,
          hidden: this.hidden,
        });
        const p = await api.get<ListPage>(`/api/fs/list?${q}`);
        this.entries = [...this.entries, ...(p.entries ?? [])];
        this.page = p.page;
        this.total = p.total;
      } catch (e) {
        this.fail(e);
      } finally {
        this.loadingMore = false;
      }
    },

    async setSort(k: SortKey) {
      if (this.sort === k) this.desc = !this.desc;
      else {
        this.sort = k;
        this.desc = false;
      }
      if (this.dir) await this.open(this.dir);
    },

    async toggleHidden() {
      this.hidden = !this.hidden;
      if (this.dir) await this.open(this.dir);
    },

    toggleSelect(name: string) {
      const i = this.selected.indexOf(name);
      if (i < 0) this.selected.push(name);
      else this.selected.splice(i, 1);
    },

    selectAll() {
      this.selected = this.allSelected ? [] : this.entries.map((e) => e.name);
    },

    clearSelection() {
      this.selected = [];
    },

    setClip(mode: ClipMode) {
      const paths = this.selectedPaths;
      if (paths.length === 0) return;
      this.clip = { mode, paths };
    },

    clearClip() {
      this.clip = null;
    },

    /**
     * 粘贴到当前目录。
     *
     * cut（剪切）成功后清剪贴板：源已经不在了，留着下一次粘贴只会报
     * "源不存在"，而用户看不出为什么。copy 则可以反复粘到不同目录 ——
     * 那正是复制的语义。
     */
    async paste(): Promise<number | null> {
      if (!this.canPaste || !this.dir) return null;
      const clip = this.clip!;
      const jobs = useFsJobsStore();
      const id = await jobs.submit({
        op: clip.mode === 'cut' ? 'move' : 'copy',
        paths: clip.paths,
        dst: this.dir,
      });
      if (clip.mode === 'cut') this.clip = null;
      this.selected = [];
      return id;
    },

    /**
     * 删除选中项（进回收站）。permanent=true 是用户在确认框里明确选了
     * "永久删除"之后的事，默认永远是可还原的那条路。
     */
    async deleteSelected(permanent = false): Promise<number | null> {
      const paths = this.selectedPaths;
      if (paths.length === 0) return null;
      const { api } = getApi();
      try {
        const r = await api.post<{ job_id: number }>('/api/fs/delete', { paths, permanent });
        this.selected = [];
        this.error = '';
        this.trashUnwritable = false;
        return r.job_id;
      } catch (e) {
        this.fail(e);
        throw e;
      }
    },

    async mkdir(name: string) {
      const { api } = getApi();
      await api.post('/api/fs/mkdir', { path: joinPath(this.dir, name) });
      this.error = '';
      await this.open(this.dir);
    },

    async rename(name: string, to: string) {
      const { api } = getApi();
      await api.post('/api/fs/rename', { from: joinPath(this.dir, name), to });
      this.error = '';
      await this.open(this.dir);
    },

    async loadTrash(): Promise<TrashItem[]> {
      const { api } = getApi();
      try {
        const r = await api.get<{ items: TrashItem[] | null }>('/api/fs/trash');
        return r.items ?? [];
      } catch (e) {
        this.fail(e);
        return [];
      }
    },

    async restoreTrash(id: string) {
      const { api } = getApi();
      await api.post(`/api/fs/trash/${encodeURIComponent(id)}/restore`);
      if (this.dir) await this.open(this.dir);
    },

    async purgeTrash(id: string) {
      const { api } = getApi();
      await api.del(`/api/fs/trash/${encodeURIComponent(id)}?confirm=1`);
    },

    async emptyTrash(): Promise<number> {
      const { api } = getApi();
      const r = await api.post<{ removed: number }>('/api/fs/trash/empty?confirm=1');
      return r.removed ?? 0;
    },

    /** 把错误落进 state。trash_unwritable 单独置旗标（理由见 state 注释）。 */
    fail(e: unknown) {
      const err = e as { message?: string; code?: string };
      this.error = err?.message || '操作失败';
      this.errorCode = err?.code ?? '';
      if (err?.code === 'trash_unwritable') this.trashUnwritable = true;
    },
  },
});

export type { ListParams };
