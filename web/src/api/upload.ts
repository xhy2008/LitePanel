// 分块上传客户端（设计 8.2 / 17 节）。
//
// 单独成模块而不是埋在组件里：这套流程里值得测的东西全在"控制流"上
// —— 断点续传只补缺的块、崩溃后不重复上传已收部分、最后一块要触发
// 收尾、上限要提前拒（不等传完 1GB 再收 413）。埋在组件里就得开着
// 浏览器测，而这些恰恰是浏览器里最难复现的几条。
//
// 后端契约的几条硬约束（都在 internal/filemgr/upload.go 里，改动会同时
// 打破这里的假设，所以逐条写明）：
//   1. 参数在头里（X-Upload-Id / X-Chunk-Index），body 是原始字节；
//      "传到哪、叫什么、多大、同名怎么办"四件事必须在第一块之前用
//      POST /upload/begin 定下来。
//   2. upload_id 由客户端生成，字符集只允许 [A-Za-z0-9_-]、长度 ≤64。
//      所以它必须是哈希，不能是文件名 —— 中文名 / 空格 / 点都会被打回 400。
//   3. GET /upload/{id}/status 与 begin **永不**报告 done=true（除非是
//      0 字节文件）。装配目标文件只在"最后一块 PUT 成功"那一次响应里
//      发生。这决定了崩溃恢复时不能"查一下状态就完事"，必须重发一块。
//   4. 同一 upload_id 重复 begin 是幂等的（如实回当前进度），但
//      dir/name/size/chunk 四项必须完全一致，否则 400。
//   5. 同名冲突策略在**第一次** begin 时就定死了：重跑时 begin 走幂等
//      分支，返回的是服务端存着的旧 meta，本次传进去的 conflict 会被
//      无声丢弃（sameIntent 不比对它）。所以"改主意要覆盖"只能先
//      DELETE 会话再重传，光换参数是不行的。

import type { Api } from './http';

export interface UploadState {
  id: string;
  name: string;
  dir: string;
  path?: string;
  size: number;
  chunk_size: number;
  received: number;
  missing?: number[];
  done: boolean;
  skipped?: boolean;
}

/**
 * 同名冲突策略。空串是后端的 ConflictAsk，意思是"没表态"。
 *
 * 默认必须是问，而不是替用户猜：默认覆盖把"界面少勾一个选项"变成静默的
 * 数据丢失；默认改名则用户以为覆盖了 a.bin、实际旧文件还在原地而新文件
 * 叫 "a (1).bin"。两种"善意的猜测"都比老实报错难解释（与后端
 * ConflictAsk 的注释同一条理由）。
 */
export type ConflictPolicy = '' | 'overwrite' | 'rename' | 'skip';

export interface UploadTask {
  name: string;
  size: number;
  /** 切片用的数据源。浏览器里传 Blob；测试里传一个函数即可。 */
  slice: (start: number, end: number) => Blob | ArrayBuffer | Uint8Array;
}

export interface UploadHooks {
  /** 已确认落盘的字节数（按块累计，单调不减）。 */
  onProgress?: (received: number) => void;
  /** 会话建立/恢复后回调一次，便于把 id 存起来供"断点续传"复用。 */
  onSession?: (id: string, chunkSize: number) => void;
  /** 每成功一块。测试用它数请求次数。 */
  onChunk?: (index: number) => void;
  signal?: AbortSignal;
}

export const DEFAULT_CHUNK = 5 << 20; // 与后端 DefaultChunkSize 对齐
export const MAX_UPLOAD_BYTES = 1 << 30; // 1GB，与设计 8.2 的上限对齐

export class UploadError extends Error {
  constructor(
    message: string,
    readonly code: string,
    readonly status: number,
  ) {
    super(message);
  }
}

/**
 * 稳定上传 ID：目标目录 + 文件名 + 大小 + 修改时间的哈希。
 *
 * 必须稳定，因为断点续传的全部前提是"同一个文件第二次传时能被认出来"。
 *
 * 为什么把**目录**也算进去：服务端按 upload_id 认会话，而同一 id 第二次
 * begin 必须四项全等（sameIntent）。少了目录，"把 report.pdf 传到 /a、
 * 再传同一个文件到 /b"会撞上同一个会话，第二次 begin 直接吃一个
 * "upload_id 已用于目录 /a" 的 400 —— 用户看到的是两次一模一样的操作,
 * 其中一次莫名其妙失败，而报错文案指向一个他根本没碰过的目录。
 *
 * 不能含文件名原文：后端只认 [A-Za-z0-9_-]，中文名会直接 400。
 * 用 FNV-1a 而不是 crypto.subtle：后者是异步的，而 ID 生成在入队的同步
 * 路径上，同步版本让整条流程保持可测（不必为它铺一套异步替身）。
 */
export function uploadIdOf(t: {
  dir: string;
  name: string;
  size: number;
  mtime?: number;
}): string {
  // 各段带长度前缀而不是用分隔符拼：目录名/文件名里出现分隔符时,
  // ("a/b", "c") 与 ("a", "b/c") 会撞出同一个 key —— 两个不同的上传
  // 共用一个会话，后到的那个会被服务端当成"偷改目标"打回 400。
  // 长度前缀让拼接单射，不依赖任何"这个名字里不会有 X"的假设。
  const key = [t.dir, t.name, String(t.size), String(t.mtime ?? 0)]
    .map((x) => `${x.length}:${x}`)
    .join('');
  let h = 0xcbf29ce484222325n;
  for (let i = 0; i < key.length; i++) {
    h ^= BigInt(key.charCodeAt(i));
    h = (h * 0x100000001b3n) & 0xffffffffffffffffn;
  }
  return h.toString(16).padStart(16, '0');
}

// 返回类型写成 BodyInit 是诚实的（浏览器 fetch 确实接受二进制），但要
// 过一道转换：happy-dom 的 BodyInit 定义里没有 ArrayBufferView，而运行时
// fetch 收 Uint8Array 是标准行为。不转换的话只能把分块全改成 Blob —— 那会
// 让浏览器为每块多做一次拷贝，而分块大小本就是按"直接 stream 到磁盘"选的。
function toBody(x: Blob | ArrayBuffer | Uint8Array): BodyInit {
  if (typeof Blob !== 'undefined' && x instanceof Blob) return x as unknown as BodyInit;
  if (x instanceof ArrayBuffer) return new Uint8Array(x) as unknown as BodyInit;
  return x as unknown as BodyInit;
}

/** 分块数。size=0 时是 0 块（后端 Begin 直接置 Done）。 */
export function chunkCount(size: number, chunk: number): number {
  if (size <= 0) return 0;
  return Math.ceil(size / chunk);
}

export interface RunUploadArgs {
  api: Api;
  /**
   * 发分块用的 fetch。必须显式注入而不是用全局：与 api/http.ts 同一套
   * 约定（那里也是 createApi({fetch}) ），否则测试就得去改 globalThis，
   * 而全局被一个用例改坏会污染同文件后面的所有用例。
   */
  fetcher: typeof globalThis.fetch;
  dir: string;
  task: UploadTask;
  id: string;
  conflict?: ConflictPolicy;
  chunkSize?: number;
  hooks?: UploadHooks;
}

/**
 * 上传一个文件，返回最终状态。
 *
 * 流程：begin → 按 missing 逐块 PUT → 最后一块的响应就是终态。
 *
 * 关键点在于"下一步传哪些块"完全由**服务端回的 missing** 决定，而不是
 * 客户端自己数：只有服务端知道暂存目录里哪些块真的落盘了。崩在两次请求
 * 之间时，客户端重跑同一套流程（同一 id、同一 begin）会拿回一份精确的
 * 缺块清单，于是"续传"不需要任何客户端状态。
 */
export async function runUpload(a: RunUploadArgs): Promise<UploadState> {
  const { api, dir, task, id, fetcher } = a;
  // 默认"问"：目标不存在就直接传，存在就吃一个 409 回到界面让用户选。
  // 不能默认 rename：那会让上面的冲突分支永远跑不到，而用户以为自己在
  // 覆盖，盘上却多出一个 "a (1).bin"。
  const conflict = a.conflict ?? '';
  const hooks = a.hooks ?? {};
  const wantChunk = a.chunkSize && a.chunkSize > 0 ? a.chunkSize : DEFAULT_CHUNK;

  if (task.size > MAX_UPLOAD_BYTES) {
    throw new UploadError(
      `文件 ${task.name} 超过 ${Math.floor(MAX_UPLOAD_BYTES / 1024 / 1024)}MB 上限，请用 SFTP/scp 传输`,
      'too_large',
      413,
    );
  }

  // begin：第一次建会话，重跑时幂等返回当前进度（含 missing）。
  let st = await beginOnce(api, id, dir, task, conflict, wantChunk);
  hooks.onSession?.(st.id, st.chunk_size);

  // 0 字节 / skip：Begin 就已经是终态，一块都不用发。
  if (st.done || st.skipped) {
    hooks.onProgress?.(st.received);
    return st;
  }

  const chunk = st.chunk_size > 0 ? st.chunk_size : wantChunk;
  const total = chunkCount(task.size, chunk);

  // 要发哪些块：服务端缺哪些就发哪些，客户端自己不数。
  //
  // missing 为空有**两种相反**的含义，必须区分开：
  //   · 全新会话（一块都没传）→ 该传全部；
  //   · 块已齐但没装配（面板崩在最后一块与 finish 之间）→ 该重发最后一块。
  // 拿 missing 的长度区分不了这两者（两种都是空），而搞错的代价是反的:
  // 前者会一个字节都不传就报成功，后者会把整份文件重传一遍。
  // 判据用 received：它是服务端从暂存目录里逐块 stat 算出来的，
  // 比任何客户端记忆都可靠。
  let toSend: number[];
  if (st.missing && st.missing.length > 0) {
    toSend = [...st.missing];
  } else if (st.received >= task.size) {
    toSend = total > 0 ? [total - 1] : [];
  } else {
    toSend = allIndexes(total);
  }

  hooks.onProgress?.(st.received);

  for (const idx of toSend) {
    if (hooks.signal?.aborted) throw new UploadError('已取消', 'canceled', 499);
    const start = idx * chunk;
    const end = Math.min(start + chunk, task.size);
    const body = toBody(task.slice(start, end));
    st = await putChunk(fetcher, id, idx, body);
    hooks.onChunk?.(idx);
    // 直接采用服务端回的 received，不本地累加：本地累加在"块被截断而
    // 服务端没报错"这类边角上会与真相分叉，代价是进度条永远到不了
    // 100%，而用户看到的就是"卡住了"。
    hooks.onProgress?.(st.received);
    // 补齐最后一块的那次响应带 done：文件已经在目标位置了。
    if (st.done || st.skipped) return st;
  }

  // 发完还没 done：清单从一开始就漏了块（比如两次请求之间服务端又收到
  // 了写入）。再问一次，按真相报错，而不是假装成功。
  const again = await api.get<UploadState>(`/api/fs/upload/${encodeURIComponent(id)}/status`);
  if (again.done || again.skipped) return again;
  if (again.missing && again.missing.length > 0) {
    throw new UploadError(`上传未收尾：仍缺 ${again.missing.length} 块`, 'incomplete', 500);
  }
  throw new UploadError('上传未收尾：暂存块已齐但目标文件未生成', 'incomplete', 500);
}

async function beginOnce(
  api: Api,
  id: string,
  dir: string,
  task: UploadTask,
  conflict: ConflictPolicy,
  chunk: number,
): Promise<UploadState> {
  return api.post<UploadState>('/api/fs/upload/begin', {
    id,
    dir,
    name: task.name,
    size: task.size,
    chunk_size: chunk,
    conflict,
  });
}

async function putChunk(
  fetcher: typeof globalThis.fetch,
  id: string,
  idx: number,
  body: BodyInit,
): Promise<UploadState> {
  // 分块的元信息在头里（设计 17 节），body 是原始字节： multipart 包一层
  // 要多一次解析与一次全量缓冲，而分块大小本来就是按"直接 stream 到磁盘"
  // 选的。代价是不能用 api.post()（它固定 JSON 编码），只能直接 fetch。
  const res = await fetcher(`/api/fs/upload`, {
    method: 'POST',
    // 写操作必须带 CSRF 头，与 http.ts 里同一个约定；漏了会 403，
    // 而 403 在上传里表现为"每个文件都传不上去"。
    headers: {
      'Content-Type': 'application/octet-stream',
      'X-Requested-With': 'litepanel',
      'X-Upload-Id': id,
      'X-Chunk-Index': String(idx),
    },
    credentials: 'same-origin',
    body,
  });
  const payload = await safeJson(res);
  if (!res.ok) {
    throw new UploadError(
      payload?.message ?? `上传失败 (${res.status})`,
      payload?.code ?? 'http_error',
      res.status,
    );
  }
  return payload as UploadState;
}

async function safeJson(res: Response): Promise<any> {
  try {
    return await res.json();
  } catch {
    return null;
  }
}

function allIndexes(n: number): number[] {
  const out = new Array<number>(n);
  for (let i = 0; i < n; i++) out[i] = i;
  return out;
}
