import { describe, it, expect, beforeEach, vi } from 'vitest';
import { setActivePinia, createPinia } from 'pinia';
import { setApi, resetApi } from '../api/inject';
import { useUploadsStore } from '../stores/uploads';
import { useFilesStore } from '../stores/files';
import type { UploadState } from '../api/upload';

// 上传队列的测试重点不是"能不能传完"（那在 upload.spec.ts 里逐块测过了）,
// 而是队列这一层独有的东西：并发上限、状态机、同 id 去重，以及
// 取消/冲突/失败这三条非 happy 路径把状态落对了没有。

function st(over: Partial<UploadState> = {}): UploadState {
  return {
    id: 'x', name: 'f', dir: '/data', size: 10, chunk_size: 5,
    received: 10, missing: [], done: true, path: '/data/f', ...over,
  };
}

function resp(o: any, status: number): Response {
  return { ok: status < 400, status, json: async () => o } as unknown as Response;
}

interface Harness {
  api: any;
  fetcher: any;
  begins: any[];
  chunks: number[];
  /** 正在飞的分块请求数（用来观察并发峰值）。 */
  inflight: number;
  peak: number;
  del: ReturnType<typeof vi.fn>;
}

/**
 * 假上传服务。
 *
 * gate 用来把分块请求挂住：并发上限这种性质，只有在"多个请求同时在飞"
 * 时看得见，而一个立刻返回的假 fetcher 永远数出峰值 1。
 */
function mkServer(opts: {
  begin?: (body: any) => UploadState;
  chunk?: (id: string, idx: number) => UploadState;
  gate?: Promise<void>;
} = {}): Harness {
  const h: Harness = {
    api: {} as any, fetcher: undefined as any,
    begins: [], chunks: [], inflight: 0, peak: 0, del: vi.fn(async () => null),
  };
  h.api = {
    // 必须回 ListPage 的形状：上传完成会刷新目录，返回一个 UploadState
    // 会把 files.dir 写成 undefined，于是"只在看着该目录时刷新"这类
    // 断言测的就不是它自己了。
    get: vi.fn(async () => ({ path: '/data', entries: [], page: 1, size: 200, total: 0 })),
    post: vi.fn(async (url: string, body?: any) => {
      if (!url.endsWith('/upload/begin')) throw new Error('unexpected POST ' + url);
      h.begins.push(body);
      if (opts.begin) return opts.begin(body);
      return st({ id: body.id, received: 0, missing: [0, 1], done: false, path: undefined });
    }),
    del: h.del,
    patch: vi.fn(), put: vi.fn(),
  };
  h.fetcher = vi.fn(async (_url: any, init: any) => {
    const id = init.headers['X-Upload-Id'];
    const idx = Number(init.headers['X-Chunk-Index']);
    h.inflight++;
    h.peak = Math.max(h.peak, h.inflight);
    try {
      if (opts.gate) await opts.gate;
      h.chunks.push(idx);
      if (opts.chunk) return resp(opts.chunk(id, idx), 200);
      return resp(st({ id, received: (idx + 1) * 5 }), 200);
    } catch (e: any) {
      return resp({ code: e?.code ?? 'boom', message: e?.message ?? '服务器炸了' }, 500);
    } finally {
      h.inflight--;
    }
  });
  return h;
}

function wire(h: Harness) {
  setApi(h.api, { pathname: '/files', search: '', assign: () => {} }, h.fetcher);
  return h;
}

function file(name = 'f.bin', size = 10, mtime = 1) {
  return { name, size, mtime, slice: (s: number, e: number) => new Uint8Array(e - s) };
}

// runOne 是 fire-and-forget（队列要并发，不能 await 一个要传一分钟的文件）,
// 所以断言前得把微任务与挂起的 promise 排空。用 while 等到没有在飞的请求
// 且没有活跃项 —— 数固定轮数是最脆弱的写法：实现多一次 await 就集体翻红,
// 少一次又可能假绿。
async function drain(u: ReturnType<typeof useUploadsStore>) {
  for (let i = 0; i < 200; i++) {
    await new Promise((r) => setTimeout(r, 0));
    if (!u.hasActive) return;
  }
  throw new Error('队列没有排空：还有 ' + u.items.filter((i) => u.items.length && i.status === 'uploading').length + ' 项在传');
}

beforeEach(() => {
  setActivePinia(createPinia());
  resetApi();
  useFilesStore().dir = '/data';
});

describe('uploads store', () => {
  it('入队后自动开传并落到 done', async () => {
    const h = wire(mkServer());
    const u = useUploadsStore();
    u.enqueue([file()]);
    await drain(u);
    expect(u.items[0].status).toBe('done');
    expect(u.items[0].received).toBe(10);
    expect(h.begins.length).toBe(1);
  });

  it('上传目标取文件页当前目录', async () => {
    const h = wire(mkServer());
    const u = useUploadsStore();
    useFilesStore().dir = '/srv/upload';
    u.enqueue([file()]);
    await drain(u);
    expect(h.begins[0].dir).toBe('/srv/upload');
  });

  // 浏览器对同域名的并发连接本就有限，开太多只会让每个文件都变慢,
  // 而且手机端上传还挤掉了同期打开页面的下载带宽。
  it('同时在传的文件数不超过 2', async () => {
    let release: () => void = () => {};
    const gate = new Promise<void>((r) => (release = r));
    const h = wire(mkServer({ gate }));
    const u = useUploadsStore();
    u.enqueue([file('a'), file('b'), file('c'), file('d')]);
    for (let i = 0; i < 5; i++) await new Promise((r) => setTimeout(r, 0));
    expect(h.peak).toBe(2); // 四个文件排队，同时只有两个在飞
    release();
    await drain(u);
    expect(h.peak).toBe(2);
    expect(u.items.every((i) => i.status === 'done')).toBe(true);
  });

  // 服务端按 upload_id 认会话。若同一个 (目录,文件) 开出两条队列项去 PUT
  // 同一个会话，后到的那条会在别人的收尾之后拿到 404 —— 界面上表现为
  // "同一个文件总有一次失败"，而两次拖入看起来一模一样。
  it('重复拖入同一个文件只有一条队列项', async () => {
    wire(mkServer());
    const u = useUploadsStore();
    u.enqueue([file()]);
    await drain(u);
    u.enqueue([file()]);
    await drain(u);
    expect(u.items.length).toBe(1);
  });

  it('不同文件是两个队列项', async () => {
    wire(mkServer());
    const u = useUploadsStore();
    u.enqueue([file('a.bin'), file('b.bin')]);
    await drain(u);
    expect(u.items.length).toBe(2);
  });

  it('目标已存在进入 conflict，且一个字节都不发', async () => {
    const h = wire(
      mkServer({
        begin: () => {
          throw { code: 'exists', status: 409, message: '/data/f.bin 已存在' };
        },
      }),
    );
    const u = useUploadsStore();
    u.enqueue([file()]);
    await drain(u);
    expect(u.items[0].status).toBe('conflict');
    // 错误原文要留住：只说"失败"，用户不知道该覆盖还是改名。
    expect(u.items[0].error).toContain('已存在');
    expect(h.chunks).toEqual([]); // 后端的 409 发生在落会话之前
  });

  it('冲突时选覆盖能接着传完', async () => {
    let ask = true;
    const h = wire(
      mkServer({
        begin: (body) => {
          if (ask && !body.conflict) {
            throw { code: 'exists', status: 409, message: '已存在' };
          }
          return st({ id: body.id, received: 0, missing: [0, 1], done: false, path: undefined });
        },
      }),
    );
    const u = useUploadsStore();
    u.enqueue([file()]);
    await drain(u);
    expect(u.items[0].status).toBe('conflict');
    ask = false;
    u.resolve(u.items[0].id, 'overwrite');
    await drain(u);
    expect(u.items[0].status).toBe('done');
    expect(h.begins.at(-1)!.conflict).toBe('overwrite');
  });

  // 取消必须顺手告诉服务端删会话。只 abort 不 DELETE，暂存目录会一直
  // 占着盘等 TTL 清扫 —— 而刚传完大文件的那个盘恰恰最不缺这点空间。
  it('取消会 DELETE 服务端会话', async () => {
    let release: () => void = () => {};
    const gate = new Promise<void>((r) => (release = r));
    const h = wire(mkServer({ gate }));
    const u = useUploadsStore();
    u.enqueue([file()]);
    const id = u.items[0].id;
    for (let i = 0; i < 5; i++) await new Promise((r) => setTimeout(r, 0));
    expect(u.items[0].status).toBe('uploading');
    const p = u.cancel(id);
    release();
    await p;
    expect(h.del).toHaveBeenCalledWith(`/api/fs/upload/${id}`);
    expect(u.items[0].status).toBe('canceled');
  });

  it('已完成的项不会被取消改写成"已取消"', async () => {
    wire(mkServer());
    const u = useUploadsStore();
    u.enqueue([file()]);
    await drain(u);
    await u.cancel(u.items[0].id);
    // 文件已经在盘上（列表刚刷出来），队列却说没传 → 用户会再传一遍。
    expect(u.items[0].status).toBe('done');
  });

  it('失败保留后端原文，重试能再排一次', async () => {
    const h = wire(
      mkServer({
        chunk: () => {
          throw { code: 'disk_full', message: 'no space left on device' };
        },
      }),
    );
    const u = useUploadsStore();
    u.enqueue([file()]);
    await drain(u);
    expect(u.items[0].status).toBe('error');
    expect(u.items[0].error).toBe('no space left on device');
    const before = h.chunks.length;
    u.retry(u.items[0].id);
    await drain(u);
    expect(h.chunks.length).toBeGreaterThan(before);
  });

  // 上限必须在发第一个请求之前就拒：传完 1GB 才收 413，用户已经白等十分钟。
  it('超过 1GB 的文件本地就拒掉，一个请求都不发', async () => {
    const h = wire(mkServer());
    const u = useUploadsStore();
    u.enqueue([file('big.bin', 2 ** 31)]); // 2GB
    await drain(u);
    expect(u.items[0].status).toBe('error');
    expect(u.items[0].error).toMatch(/SFTP|上限/);
    expect(h.begins).toEqual([]);
    expect(h.chunks).toEqual([]);
  });

  // 无脑刷新会把他正在翻的目录弹回第一页：刚滚到第 400 项找东西,
  // 身后传完一个文件就把列表弹回顶部。
  it('传完只在"用户正看着那个目录"时刷新列表', async () => {
    let release: () => void = () => {};
    const h = wire(mkServer({ gate: new Promise<void>((r) => (release = r)) }));
    const f = useFilesStore();
    const spy = vi.spyOn(f, 'open').mockResolvedValue(undefined);
    const u = useUploadsStore();
    // 入队时目录是 /data；趁它还在传，用户翻到别处 —— 传完不该动他眼前的列表。
    // 必须用 gate 卡住：不卡的话上传在这几行之前就完事了，测的是竞态而不是逻辑。
    u.enqueue([file()]);
    for (let i = 0; i < 3; i++) await new Promise((r) => setTimeout(r, 0));
    f.dir = '/elsewhere';
    release();
    await drain(u);
    expect(u.items[0].status).toBe('done');
    expect(spy).not.toHaveBeenCalled();

    // 反过来：正看着目标目录时必须刷新，否则用户看不到刚传完的文件。
    f.dir = '/data';
    u.enqueue([file('other.bin')]);
    await drain(u);
    expect(spy).toHaveBeenCalledWith('/data');
  });

  it('clearFinished 只留下还在跑的', async () => {
    let release: () => void = () => {};
    const gate = new Promise<void>((r) => (release = r));
    wire(mkServer({ gate }));
    const u = useUploadsStore();
    u.enqueue([file('done.bin')]);
    // 手工把第一项标成完成，第二项留在队列里当"还在跑"。
    u.items[0].status = 'done';
    u.enqueue([file('pending.bin')]);
    u.clearFinished();
    expect(u.items.map((i) => i.name)).toEqual(['pending.bin']);
    release();
  });

  it('pendingCount 把待处理的冲突也算进去', async () => {
    wire(
      mkServer({
        begin: () => {
          throw { code: 'exists', status: 409, message: '已存在' };
        },
      }),
    );
    const u = useUploadsStore();
    u.enqueue([file()]);
    await drain(u);
    // 等用户表态的冲突不是"完成"，角标必须还亮着 —— 否则用户翻到别的
    // 页面就再也不会想起有个上传卡在同名冲突上。
    expect(u.pendingCount).toBe(1);
  });
});
