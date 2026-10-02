import { describe, it, expect, vi } from 'vitest';
import { runUpload, uploadIdOf, chunkCount, MAX_UPLOAD_BYTES, UploadError } from '../api/upload';
import type { UploadState } from '../api/upload';

// 内存版假后端，语义严格照 internal/filemgr/upload.go 抄：
//   · begin 幂等（同 id 重复 begin 回当前进度），dir/name/size 不一致 400
//   · status 与 begin **永不**报 done；装配只在"补齐最后一块"那次 PUT 的响应里
//   · received 只累加"长度正好等于该块应有长度"的块（真实现逐块 stat 比对）
//   · 0 字节文件 Begin 直接 Done
// 手写返回值也能测，但"忘了某条契约"的测试会跟着一起忘；让替身跟着真实现
// 走，替身自己就成了第二份规格。
class FakeServer {
  sessions = new Map<string, { dir: string; name: string; size: number; chunk: number }>();
  chunks = new Map<string, Map<number, Uint8Array>>();
  crashAt = -1; // 发够 N 块之后网络断
  putCalls: number[] = [];
  beginCalls = 0;
  statusCalls = 0;
  private expected(id: string, i: number): number {
    const s = this.sessions.get(id)!;
    return Math.min(s.chunk, s.size - i * s.chunk);
  }

  private missingOf(id: string): number[] {
    const s = this.sessions.get(id);
    if (!s) return [];
    const have = this.chunks.get(id) ?? new Map();
    const miss: number[] = [];
    for (let i = 0; i < chunkCount(s.size, s.chunk); i++) {
      const b = have.get(i);
      if (!b || b.length !== this.expected(id, i)) miss.push(i);
    }
    return miss;
  }

  private receivedOf(id: string): number {
    const have = this.chunks.get(id);
    if (!have) return 0;
    let t = 0;
    for (const [i, b] of have) if (b.length === this.expected(id, i)) t += b.length;
    return t;
  }

  begin(body: any): UploadState {
    this.beginCalls++;
    const ex = this.sessions.get(body.id);
    if (ex) {
      if (ex.dir !== body.dir || ex.name !== body.name || ex.size !== body.size) {
        throw { code: 'bad_path', status: 400, message: 'upload_id 已用于别的目标' };
      }
    } else {
      this.sessions.set(body.id, { dir: body.dir, name: body.name, size: body.size, chunk: body.chunk_size });
      this.chunks.set(body.id, new Map());
    }
    // 0 字节：没有块可传，Begin 之后直接完成（真实现就地 finish）。
    if (body.size === 0) {
      return {
        id: body.id, name: body.name, dir: body.dir, path: `${body.dir}/${body.name}`,
        size: 0, chunk_size: body.chunk_size, received: 0, missing: [], done: true,
      };
    }
    // 非 0 字节时 begin **永不**报 done。
    return this.stateOf(body.id, false);
  }

  stateOf(id: string, done: boolean): UploadState {
    const s = this.sessions.get(id)!;
    return {
      id, name: s.name, dir: s.dir,
      path: done ? `${s.dir}/${s.name}` : undefined,
      size: s.size, chunk_size: s.chunk,
      received: this.receivedOf(id), missing: this.missingOf(id), done,
    };
  }

  put(id: string, idx: number, bytes: Uint8Array): UploadState {
    if (this.crashAt >= 0 && this.putCalls.length >= this.crashAt) {
      throw new TypeError('Failed to fetch'); // 网络断，与服务端报错不同类
    }
    this.putCalls.push(idx);
    if (!this.sessions.has(id)) throw { code: 'not_found', status: 404, message: '没有这个上传会话' };
    this.chunks.get(id)!.set(idx, bytes);
    // 装配只发生在"补齐最后一块"这一次 PUT 的响应里。
    return this.stateOf(id, this.missingOf(id).length === 0);
  }

  status(id: string): UploadState {
    this.statusCalls++;
    return this.stateOf(id, false); // 状态查询永不报完成（与后端一致）
  }
}

function task(size: number, name = 'f.bin') {
  return {
    name,
    size,
    // 切出真实长度的片段，让假后端能校验"块长度对不对"。
    slice: (s: number, e: number) => new Uint8Array(e - s),
  };
}

function mkHarness(srv: FakeServer, fetchOverride?: any) {
  const api = {
    get: vi.fn(async (url: string) => srv.status(decodeURIComponent(url.split('/upload/')[1].split('/')[0]))),
    post: vi.fn(async (url: string, body?: any) => {
      if (url.endsWith('/upload/begin')) return srv.begin(body);
      throw new Error('unexpected POST ' + url);
    }),
    del: vi.fn(), patch: vi.fn(), put: vi.fn(),
  };
  const fetcher =
    fetchOverride ??
    vi.fn(async (_url: any, init: any) => {
      const id = init.headers['X-Upload-Id'];
      const idx = Number(init.headers['X-Chunk-Index']);
      const raw = init.body as Uint8Array;
      try {
        return json(srv.put(id, idx, raw), 200);
      } catch (e: any) {
        if (e instanceof TypeError) throw e; // 网络错：照实抛出
        return json(e, e.status ?? 500);
      }
    });
  // 断言分块请求真的带了那两个头（后端缺任一个都回 400，而 400 在界面上
  // 表现为"每个文件都传不上去"，看不出是头漏了）。
  const lastHeaders = () => (fetcher as any).mock.calls.at(-1)?.[1]?.headers ?? {};
  return { api: api as any, fetcher: fetcher as any, lastHeaders };
}

function json(o: any, status: number): Response {
  return { ok: status < 400, status, json: async () => o } as unknown as Response;
}

describe('uploadIdOf', () => {
  it('同一文件两次得到同一个 id（断点续传的全部前提）', () => {
    const a = { dir: '/data', name: 'a.tar.gz', size: 123, mtime: 456 };
    expect(uploadIdOf(a)).toBe(uploadIdOf({ ...a }));
  });

  // 后端只允许 [A-Za-z0-9_-] 且 ≤64 字节：中文名/空格/点都会直接 400。
  it('中文名与空格也产出合法 id', () => {
    const id = uploadIdOf({
      dir: '/data/我的 目录',
      name: '我的 文件 v2 (1).tar.gz',
      size: 1,
      mtime: 2,
    });
    expect(id).toMatch(/^[A-Za-z0-9_-]{1,64}$/);
  });

  it('不同文件不同 id（撞 id 会被后端判为"改目标"而 400）', () => {
    const base = { dir: '/d', name: 'a', size: 1 };
    expect(uploadIdOf(base)).not.toBe(uploadIdOf({ ...base, name: 'b' }));
    expect(uploadIdOf(base)).not.toBe(uploadIdOf({ ...base, size: 2 }));
  });

  // 拼接不能有歧义：目录名以 "/a" 结尾 + 文件 "b" 与目录 "/" + 文件
  // "a/b" 用分隔符拼会撞同一个 key，于是两个不相干的上传共用一个会话。
  it('目录与文件名的边界不会撞 id', () => {
    expect(uploadIdOf({ dir: '/x/a', name: 'b', size: 1 })).not.toBe(
      uploadIdOf({ dir: '/x', name: 'a/b', size: 1 }),
    );
  });

  // 同一文件传到不同目录必须是两个独立会话。少了这一条，第二次 begin
  // 会被后端的 sameIntent 判成"偺改目标"而 400：用户做了两次同样的操作,
  // 一次成功一次失败，而报错文案指向一个他没碰过的目录。
  it('同一文件传到不同目录是两个不同的 id', () => {
    const f = { name: 'report.pdf', size: 10, mtime: 1 };
    expect(uploadIdOf({ ...f, dir: '/a' })).not.toBe(uploadIdOf({ ...f, dir: '/b' }));
  });
});

describe('chunkCount', () => {
  it('0 字节是 0 块（后端 Begin 直接 Done）', () => expect(chunkCount(0, 5)).toBe(0));
  it('整除不多切一块', () => expect(chunkCount(10, 5)).toBe(2));
  it('有余数多一块', () => expect(chunkCount(11, 5)).toBe(3));
});

describe('runUpload', () => {
  it('小文件按序传完，最后一块的响应就是 done', async () => {
    const srv = new FakeServer();
    const { api, fetcher, lastHeaders } = mkHarness(srv);
    const st = await runUpload({ api, fetcher, dir: '/data', id: 'u1', task: task(3, 'x.txt'), chunkSize: 2 });
    expect(st.done).toBe(true);
    expect(st.path).toBe('/data/x.txt');
    expect(srv.putCalls).toEqual([0, 1]);
    expect(lastHeaders()['X-Upload-Id']).toBe('u1');
    expect(lastHeaders()['X-Chunk-Index']).toBe('1');
  });

  it('最后一块只切余下的长度（切错会把文件写坏）', async () => {
    const srv = new FakeServer();
    const seen: number[] = [];
    const api = {
      get: vi.fn(), del: vi.fn(), patch: vi.fn(), put: vi.fn(),
      post: vi.fn(async (url: string, body: any) => {
        if (url.endsWith('/upload/begin')) return srv.begin(body);
        throw new Error('unexpected');
      }),
    };
    const fetcher = vi.fn(async (_u: any, init: any) => {
      const bytes = init.body as Uint8Array;
      seen.push(bytes.length);
      return json(srv.put(init.headers['X-Upload-Id'], Number(init.headers['X-Chunk-Index']), bytes), 200);
    });
    await runUpload({
      api: api as any, fetcher: fetcher as any, dir: '/d', id: 'uL',
      task: { name: 'f', size: 5, slice: (s, e) => new Uint8Array(e - s) }, chunkSize: 2,
    });
    expect(seen).toEqual([2, 2, 1]); // 2+2+1 = 5，最后一块只有 1 字节
  });

  it('0 字节文件不发任何块', async () => {
    const srv = new FakeServer();
    const { api, fetcher } = mkHarness(srv);
    const st = await runUpload({ api, fetcher, dir: '/data', id: 'u0', task: task(0, 'empty') });
    expect(st.done).toBe(true);
    expect(srv.putCalls).toEqual([]);
  });

  // 续传的全部意义：已收到的块不许重传。真设备上重传几个 GB 是几分钟白干，
  // 而且会让进度条倒退。
  it('续传只补缺的块，已收到的一个都不重发', async () => {
    const srv = new FakeServer();
    seed(srv, 'u2', '/data', 'big.bin', 10, 2, [0, 1]);
    const { api, fetcher } = mkHarness(srv);
    const st = await runUpload({ api, fetcher, dir: '/data', id: 'u2', task: task(10, 'big.bin'), chunkSize: 2 });
    expect(srv.putCalls).toEqual([2, 3, 4]);
    expect(st.done).toBe(true);
  });

  it('崩溃后重跑：同一 id、begin 幂等、接着传完', async () => {
    const srv = new FakeServer();
    const t = task(10, 'big.bin');
    const a1 = mkHarness(srv);
    srv.crashAt = 2;
    await expect(
      runUpload({ api: a1.api, fetcher: a1.fetcher, dir: '/data', id: 'u3', task: t, chunkSize: 2 }),
    ).rejects.toBeTruthy();
    expect(srv.putCalls).toEqual([0, 1]);

    // 重跑时客户端不存任何进度，全靠服务端回的 missing。
    srv.crashAt = -1;
    const a2 = mkHarness(srv);
    const st = await runUpload({ api: a2.api, fetcher: a2.fetcher, dir: '/data', id: 'u3', task: t, chunkSize: 2 });
    expect(st.done).toBe(true);
    expect(srv.putCalls).toEqual([0, 1, 2, 3, 4]); // 前两块没重发
  });

  it('目标不一致时服务端拒绝复用 id，错误原样抛出', async () => {
    const srv = new FakeServer();
    seed(srv, 'u4', '/etc', 'passwd', 10, 2, []);
    const { api, fetcher } = mkHarness(srv);
    await expect(
      runUpload({ api, fetcher, dir: '/data', id: 'u4', task: task(10) }),
    ).rejects.toMatchObject({ code: 'bad_path' });
  });

  // 上限必须在发第一块之前就拒：等传完 1GB 再收 413，用户已经白等十分钟。
  it('超过 1GB 在 begin 之前就拒', async () => {
    const srv = new FakeServer();
    const { api, fetcher } = mkHarness(srv);
    await expect(
      runUpload({ api, fetcher, dir: '/data', id: 'u5', task: task(MAX_UPLOAD_BYTES + 1) }),
    ).rejects.toBeInstanceOf(UploadError);
    expect(srv.beginCalls).toBe(0);
    expect(srv.putCalls).toEqual([]);
  });

  it('服务端报错（413）带着 code 抛出，界面才能区别对待', async () => {
    const srv = new FakeServer();
    const fetcher = vi.fn(async () => json({ code: 'too_large', message: '太大' }, 413));
    const { api } = mkHarness(srv);
    await expect(
      runUpload({ api, fetcher: fetcher as any, dir: '/data', id: 'u6', task: task(4, 'a'), chunkSize: 2 }),
    ).rejects.toMatchObject({ code: 'too_large', status: 413 });
  });

  it('进度单调不减，最终等于文件大小', async () => {
    const srv = new FakeServer();
    const { api, fetcher } = mkHarness(srv);
    const seen: number[] = [];
    await runUpload({
      api, fetcher, dir: '/d', id: 'u7', task: task(6, 'p'), chunkSize: 2,
      hooks: { onProgress: (r) => seen.push(r) },
    });
    expect(seen.every((v, i) => i === 0 || v >= seen[i - 1])).toBe(true);
    expect(seen[seen.length - 1]).toBe(6);
  });

  it('取消信号在块边界生效，不再发下一块', async () => {
    const srv = new FakeServer();
    const { api, fetcher } = mkHarness(srv);
    const ac = new AbortController();
    await expect(
      runUpload({
        api, fetcher, dir: '/d', id: 'u8', task: task(100, 'p'), chunkSize: 2,
        hooks: { signal: ac.signal, onChunk: () => ac.abort() },
      }),
    ).rejects.toMatchObject({ code: 'canceled' });
    expect(srv.putCalls.length).toBeLessThan(50);
  });

  // 块已齐但没装配（面板崩在最后一块与 finish 之间）。此时 status 永不报
  // done，客户端若就此放弃，暂存块会等一个 TTL 被扫掉，而界面显示"已传完"
  // —— 目标目录里其实什么都没有。唯一能触发后端收尾的动作是再 PUT 一块。
  it('块已齐但未装配时，重发最后一块去触发收尾', async () => {
    const srv = new FakeServer();
    // 三块全在盘上 → missing 为空，而会话从未 finish 过（begin/status
    // 都如实回 done=false）。这就是"崩在最后一块与 finish 之间"。
    seed(srv, 'u9', '/d', 'f', 6, 2, [0, 1, 2]);
    const { api, fetcher } = mkHarness(srv);
    const st = await runUpload({ api, fetcher, dir: '/d', id: 'u9', task: task(6, 'f'), chunkSize: 2 });
    expect(st.done).toBe(true);
    expect(srv.putCalls).toEqual([2]); // 只重发最后一块，不是整份重传
  });

  // "全新会话"与"块已齐等收尾"的 missing 都是空数组，两者的正确动作相反
  // （发全部 vs 只补最后一块）。拿 received 区分不了就两个都错。
  it('missing 为空但 received 为 0 时按全新会话传全部', async () => {
    const srv = new FakeServer();
    seed(srv, 'uA', '/d', 'f', 6, 2, []);
    const { api, fetcher } = mkHarness(srv);
    const st = await runUpload({ api, fetcher, dir: '/d', id: 'uA', task: task(6, 'f'), chunkSize: 2 });
    expect(st.done).toBe(true);
    expect(srv.putCalls).toEqual([0, 1, 2]);
  });
});

// 预置一个"上次传到一半"的会话：have 里的块写成正确长度，
// 让后端的逐块长度校验认它们为已收。
function seed(
  srv: FakeServer, id: string, dir: string, name: string,
  size: number, chunk: number, have: number[],
) {
  srv.sessions.set(id, { dir, name, size, chunk });
  const m = new Map<number, Uint8Array>();
  for (const i of have) {
    const len = Math.min(chunk, size - i * chunk);
    m.set(i, new Uint8Array(len));
  }
  srv.chunks.set(id, m);
}
