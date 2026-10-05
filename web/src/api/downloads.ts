// 下载任务的 API 类型与调用。字段与 internal/download 的 JSON tag 逐字段
// 对齐：改名就会与 HTTP 返回悄悄漂移，这类漂移只会在运行时炸。

export type DlState =
  | 'active'
  | 'waiting'
  | 'paused'
  | 'error'
  | 'complete'
  | 'removed';

export interface DlTask {
  gid: string;
  uris: string[];
  name?: string;
  dir?: string;
  state: DlState;
  error?: string;
  total_bytes: number;
  done_bytes: number;
  speed: number;
  connections: number;
  created_at: number;
  finished_at?: number;
  /** aria2 拒绝控制（GM 离线任务不可 remove）时为 false，按钮要禁用。 */
  can_control: boolean;
}

export interface DlSummary {
  speed: number;
  active: number;
  waiting: number;
  paused: number;
  done: number;
  failed: number;
}

export interface DlHealth {
  ok: boolean;
  version?: string;
  message?: string;
  checked_at: number;
}

/** WS 'downloads' 频道的事件（internal/download.Event）。 */
export interface DlEvent {
  kind: 'started' | 'completed' | 'error' | 'paused' | 'stopped';
  gid: string;
  error?: string;
  at: number;
}

export interface DlAddInput {
  uris: string[];
  /** 留空 = 用 aria2/设置里的默认目录（后端 normalizeDownloadDir 对空串返空串）。 */
  dir?: string;
  out?: string;
  split?: number;
}

import { getApi } from './inject';

export async function fetchTasks(): Promise<DlTask[]> {
  const { api } = getApi();
  const r = await api.get<{ tasks: DlTask[] }>('/api/dl/tasks');
  // 后端已保证数组；这里再兜一层是因为 view 层处处 tasks.length ——
  // 一个 null 会让整页白屏，而代价只是一次 `?? []`。
  return r.tasks ?? [];
}

export async function fetchSummary(): Promise<DlSummary> {
  const { api } = getApi();
  return api.get<DlSummary>('/api/dl/summary');
}

export async function fetchHealth(): Promise<DlHealth> {
  const { api } = getApi();
  return api.get<DlHealth>('/api/dl/health');
}

export async function addTask(inpt: DlAddInput): Promise<DlTask> {
  const { api } = getApi();
  return api.post<DlTask>('/api/dl/tasks', inpt);
}

export async function pauseTask(gid: string): Promise<void> {
  const { api } = getApi();
  await api.post(`/api/dl/tasks/${gid}/pause`);
}

export async function resumeTask(gid: string): Promise<void> {
  const { api } = getApi();
  await api.post(`/api/dl/tasks/${gid}/resume`);
}

/** force=true 时 aria2 强删（会留下 .aria2 控制文件，后端要求显式选择）。 */
export async function removeTask(gid: string, force: boolean): Promise<void> {
  const { api } = getApi();
  await api.del(`/api/dl/tasks/${gid}${force ? '?force=true' : ''}`);
}

/** 清历史：只动完成/失败的，返回清掉的条数。 */
export async function clearHistory(): Promise<number> {
  const { api } = getApi();
  const r = await api.del<{ cleared: number }>('/api/dl/history');
  return r.cleared ?? 0;
}

export function speedLabel(bps: number): string {
  if (bps <= 0) return '';
  if (bps < 1024) return `${bps} B/s`;
  if (bps < 1024 * 1024) return `${(bps / 1024).toFixed(1)} KB/s`;
  return `${(bps / 1024 / 1024).toFixed(1)} MB/s`;
}

/**
 * 任务标题：后端 name 优先（来自 out 或首个文件的 basename），否则从首个 URI
 * 提文件名。magnet 在元数据到手前没有文件名，给它一个可读的占位而不是把
 * 120 字符的 magnet 链接当标题。解码失败（%zz 这种）用原文：宁可丑。
 */
export function displayName(t: DlTask): string {
  if (t.name) return t.name;
  const uri = t.uris[0] ?? '';
  if (!uri) return '(无地址)';
  if (uri.startsWith('magnet:')) return 'BT 下载（元数据获取中）';
  try {
    const path = new URL(uri).pathname;
    const base = path.split('/').filter(Boolean).pop();
    if (base) return decodeURIComponent(base);
  } catch {
    /* 非 URL（bt:// 等）落到下面的截断 */
  }
  return uri.length > 60 ? uri.slice(0, 60) + '…' : uri;
}

