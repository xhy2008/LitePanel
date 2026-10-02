// 与后端 internal/filemgr/jobs.go 的 Job / notify.go 的 JobProgress 一一对应。
// 字段保持 snake_case 同名（与 services.ts 同一条理由）：HTTP 与 WS 推的是
// 同一个类型，前端自己改名就会与其中一边悄悄漂移。

export type JobOp = 'copy' | 'move' | 'delete';

export type JobState =
  | 'pending'
  | 'running'
  | 'done'
  | 'failed'
  | 'canceled'
  | 'interrupted';

export interface JobRow {
  id: number;
  op: JobOp;
  src: string[];
  dst: string;
  // total_bytes 目前后端**恒为 0**（受理时不扫源，见 handlers_jobs.go 的
  // 校验边界注释）。进度一律以 entries_* 为准，字节只在 >0 时才显示。
  total_bytes: number;
  done_bytes: number;
  entries_total: number;
  entries_done: number;
  state: JobState;
  cancel_requested: boolean;
  permanent: boolean;
  resumed: boolean;
  // 只有失败/中断时非空。后端 json 上是 omitempty，所以这里必须可选。
  error?: string;
  created_at: number;
  updated_at: number;
}

// WS fsjobs 频道推的是整行的有界子集（不含 src，载荷必须有上界）。
// 字段名与 JobRow 同名的断言在后端钉着（TestNotifyFieldNamesMatchJobJSON）。
export interface JobProgress {
  id: number;
  op: JobOp;
  dst: string;
  total_bytes: number;
  done_bytes: number;
  entries_total: number;
  entries_done: number;
  state: JobState;
  cancel_requested: boolean;
  permanent: boolean;
  resumed: boolean;
  error?: string;
  updated_at: number;
}

export interface JobInput {
  op: JobOp;
  paths: string[];
  dst?: string;
  permanent?: boolean;
}

/** 进行中 = 还占着 worker 或随时会被领走，抽屉据此决定要不要继续轮询。 */
export function isActive(state: JobState): boolean {
  return state === 'pending' || state === 'running';
}

/**
 * 进度百分比。
 *
 * 只有条目数可靠：total_bytes 现在恒为 0（受理时不扫源），拿它当分母
 * 会算出 NaN，而 NaN 进了 style 的 width 会让整条进度条消失 —— 看起来
 * 像"任务没在动"，其实是界面算不出来。分母为 0 时回 null，让调用方
 * 明确显示"进度未知"而不是画一条 0% 的死条。
 */
export function jobPercent(j: JobRow | JobProgress): number | null {
  if (j.entries_total > 0) {
    const p = Math.round((j.entries_done / j.entries_total) * 100);
    return Math.max(0, Math.min(100, p));
  }
  if (j.total_bytes > 0) {
    const p = Math.round((j.done_bytes / j.total_bytes) * 100);
    return Math.max(0, Math.min(100, p));
  }
  return null;
}

export const OP_LABEL: Record<JobOp, string> = {
  copy: '复制',
  move: '移动',
  delete: '删除',
};
