import { defineStore } from 'pinia';
import { getApi } from '../api/inject';
import { runUpload, uploadIdOf, UploadError } from '../api/upload';
import type { ConflictPolicy, UploadTask } from '../api/upload';
import { useFilesStore } from './files';

// 上传队列。
//
// 为什么单独成一个 store、而不并进 fsJobs：上传**不是**后端的任务。
// 它是浏览器驱动的分块 HTTP 协议（设计 8.2），进度只存在于这个标签页里,
// 关掉页面就停 —— 而 fs_jobs 里的任务是服务端跑、跨标签页、WS 推送的。
// 把两者混进一个列表会出现"看起来能重试、其实关掉浏览器就没了"的假任务。
//
// 为什么不塞进 files store：files store 管的是"当前目录看到什么"，
// 上传项的生命周期跨目录（在 A 目录发起的上传，用户翻到 B 目录还在传）。

export type UploadStatus = 'queued' | 'uploading' | 'conflict' | 'done' | 'error' | 'canceled';

export interface UploadItem {
  id: string;
  dir: string;
  name: string;
  size: number;
  mtime?: number;
  received: number;
  status: UploadStatus;
  error?: string;
  /** 已定下的同名冲突策略；undefined = 还没表态（后端会替我们拒绝）。 */
  conflict?: ConflictPolicy;
  /** 完成后的最终路径。 */
  path?: string;
  /** 被跳过（目标已存在 + 跳过策略）。 */
  skipped?: boolean;
  /** 供"断点续传"复用的会话 id（= uploadIdOf 的结果，同一个文件恒定）。 */
  slice: (start: number, end: number) => Blob | ArrayBuffer | Uint8Array;
}

interface State {
  items: UploadItem[];
  error: string;
}

/**
 * 同时在传的文件数。
 *
 * 1 个太慢（手机上多选十个文件要排一分钟），太多则每个文件都变慢、
 * 而且浏览器对同域名的并发连接本来就有限。2 是"看起来同时在动"的
 * 最小值，也是一块网卡上带宽浪费最少的折中。
 */
const CONCURRENCY = 2;

// 见文件末尾 aborters 的说明。
const aborters = new Map<string, AbortController>();

export const useUploadsStore = defineStore('uploads', {
  state: (): State => ({ items: [], error: '' }),

  getters: {
    activeCount(s): number {
      return s.items.filter((i) => i.status === 'uploading' || i.status === 'queued').length;
    },
    hasActive(s): boolean {
      return s.items.some((i) => i.status === 'uploading' || i.status === 'queued');
    },
    /** 界面角标：还没落地的那些（含等待用户处理同名冲突的）。 */
    pendingCount(s): number {
      return s.items.filter(
        (i) => i.status !== 'done' && i.status !== 'canceled',
      ).length;
    },
  },

  actions: {
    /** 收一批文件入队，随后自动开传。 */
    enqueue(files: Array<{ name: string; size: number; mtime?: number; slice: UploadTask['slice'] }>) {
      const files0 = useFilesStore();
      for (const f of files) {
        const id = uploadIdOf({ dir: files0.dir, ...f });
        // 同名同大小的文件第二次拖进来，如果上一轮还在传，就复用那一项:
        // 服务端按 upload_id 认会话，插入第二条只会让两条队列去 PUT 同一
        // 个会话，然后其中一条在别人的收尾之后拿到一个 404。
        const ex = this.items.find((i) => i.id === id && i.dir === files0.dir);
        if (ex) {
          if (ex.status === 'done' || ex.status === 'canceled') {
            ex.status = 'queued';
            ex.received = 0;
            ex.error = undefined;
          }
          continue;
        }
        this.items.push({
          id,
          dir: files0.dir,
          name: f.name,
          size: f.size,
          mtime: f.mtime,
          received: 0,
          status: 'queued',
          slice: f.slice,
        });
      }
      void this.pump();
    },

    /** 起够并发数的队列项。跑完一个自动补一个。 */
    async pump() {
      const running = this.items.filter((i) => i.status === 'uploading').length;
      let slots = CONCURRENCY - running;
      if (slots <= 0) return;
      for (const it of this.items) {
        if (slots <= 0) break;
        if (it.status !== 'queued') continue;
        slots--;
        // 不 await：队列是并发的，等一个文件传完才起下一个等于把并发
        // 变成串行。错误在 runOne 里落进 item，不会变成 unhandled。
        void this.runOne(it.id);
      }
    },

    /**
     * 传一个文件。
     *
     * 这里唯一的分支是"服务端说目标已存在"：那不是什么失败，而是后端
     * 拒绝替用户猜（设计里 ConflictAsk 的语义）。界面要摆出三条出路,
     * 而不是弹一句"上传失败"。
     */
    async runOne(id: string) {
      const it = this.items.find((i) => i.id === id);
      if (!it || it.status !== 'queued') return;
      const { api, fetch: f } = getApi();
      it.status = 'uploading';
      it.error = undefined;
      const ac = new AbortController();
      aborters.set(id, ac);
      try {
        const st = await runUpload({
          api,
          fetcher: f,
          dir: it.dir,
          id: it.id,
          conflict: it.conflict,
          task: { name: it.name, size: it.size, slice: it.slice },
          hooks: {
            signal: ac.signal,
            // 直接采用服务端回的 received：它是服务端从暂存目录逐块 stat
            // 算出来的，比客户端本地累加更可靠（块被截断时本地累加会虚高,
            // 于是进度条到了 100% 而文件其实缺一段）。
            //
            // 不做"只准前进"的钳制 backpage：成功的 PutChunk 响应里 received
            // 本就单调（后端 writeChunk 对长度不符直接报 ErrChunkLength,
            // 不会回一个更小的状态）。真出现倒退只能是后端的回归 —— 那时
            // 该让进度条如实回缩并暴露问题，而不是用钳制把它藏起来。
            onProgress: (r) => { it.received = r; },
          },
        });
        it.received = st.size;
        it.path = st.path;
        it.skipped = st.skipped;
        it.status = 'done';
        this.refresh(it.dir);
      } catch (e) {
        const ue = e as UploadError;
        if (ue?.code === 'exists') {
          // 会话还没建（ask 的 409 发生在落盘之前），所以 resolveConflict
          // 直接换策略重传就行，不用先 DELETE 会话。
          it.status = 'conflict';
          it.error = ue.message;
        } else if (ue?.code === 'canceled') {
          it.status = 'canceled';
        } else {
          it.status = 'error';
          // 必须把后端的原文留住（"no space left on device" 之类）：
          // 只剩一句"失败"，用户分不清该重试还是该清盘。
          it.error = ue?.message ?? String(e);
        }
      } finally {
        aborters.delete(id);
        void this.pump();
      }
    },

    /**
     * 用户处理同名冲突。
     *
     * 三条出路来自后端的三个策略，语义各不相同：覆盖是替换、重命名是
     * 让位到 "name (1).ext"、跳过是一个字节都不传。少了任何一条,
     * 界面上就有一种"只能取消"的死局（比如目标盘满了想覆盖掉旧的）。
     */
    resolve(id: string, policy: ConflictPolicy) {
      const it = this.items.find((i) => i.id === id);
      if (!it) return;
      it.conflict = policy;
      it.status = 'queued';
      it.error = undefined;
      void this.pump();
    },

    /** 重新排队（失败或取消之后）。同一 upload_id 会接着缺块传。 */
    retry(id: string) {
      const it = this.items.find((i) => i.id === id);
      if (!it) return;
      it.status = 'queued';
      it.error = undefined;
      void this.pump();
    },

    /**
     * 取消：先掐本地请求，再告诉服务端把会话删掉。
     *
     * 顺序很重要。只 abort 不 DELETE 的话，暂存目录会一直占着盘直到
     * TTL 清扫 —— 而用户刚上传大文件的那个盘，恰恰最不缺这点空间。
     * DELETE 失败不报给用户：他要的效果（停止）已经达成了。
     */
    async cancel(id: string) {
      const it = this.items.find((i) => i.id === id);
      if (!it) return;
      const ac = aborters.get(id);
      ac?.abort();
      // 排队中的项没有请求在飞，直接改状态即可。
      // 已经传完的项不算"取消"——文件已经在盘上（列表刚刷出来）,
      // 队列却说没传，用户会再传一遍。它只能被 remove() 掉。
      if (it.status !== 'uploading' && it.status !== 'done') it.status = 'canceled';
      try {
        const { api } = getApi();
        await api.del(`/api/fs/upload/${encodeURIComponent(id)}`);
      } catch {
        /* 服务端可能已经扫掉了这个会话；没什么可补救的 */
      }
      // 兜最后一次状态：abort 与"最后一个块刚好返回"能并发走到这里,
      // 而那时 runOne 的 catch 不会跑，状态就永远停在 uploading。
      // 仍然不能覆盖 done，理由同上。
      if (it.status === 'uploading') it.status = 'canceled';
    },

    remove(id: string) {
      this.items = this.items.filter((i) => i.id !== id);
    },

    clearFinished() {
      this.items = this.items.filter((i) => i.status === 'uploading' || i.status === 'queued');
    },

    /**
     * 传完后刷新目录 —— 但只在用户正看着那个目录时。
     *
     * 无脑刷新会把他正在翻的目录重置回第一页：他刚滚到第 400 项找东西,
     * 身后传完一个文件就把列表弹回顶部。
     */
    refresh(dir: string) {
      const f = useFilesStore();
      if (f.dir && f.dir === dir) void f.open(dir);
    },
  },

});

// 在飞请求的取消句柄。放在模块级而不是 state 里：AbortController 不是
// 响应式数据，塞进 state 会被 Vue 包一层代理，devtools 做 state 快照时
// 会碰到里面的原生句柄。
//
// 跨测试共享这一个 Map 是安全的：runOne 一定在发请求前先 set 自己的句柄,
// 所以上一个用例残留的条目会被覆盖；而残留的那个请求早就结束了,
// 多 abort 一次没有任何副作用。
