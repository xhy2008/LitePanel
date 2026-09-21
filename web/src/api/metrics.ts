// 与后端 internal/metrics/snapshot.go 的 JSON tag 一一对应。
// 后端字段用 snake_case（Go struct tag），这里保持同名 —— 前端不做
// camelCase 转换的原因：HTTP 与 WS 两条路序列化的是同一个 Go 类型，
// 一旦前端自己改名就会与其中一条悄悄漂移。
export interface DiskUsage {
  mountpoint: string;
  device: string;
  fstype: string;
  total: number;
  used: number;
  free: number;
  percent: number;
}

export interface MemStat {
  total: number;
  available: number;
  used: number;
  percent: number;
  swap_total: number;
  swap_used: number;
  swap_percent: number;
}

export interface CpuStat {
  percent: number | null;
  // 逐核占用百分比数组，采集器首次差分未建立时省略。
  cores?: number[];
  load1: number;
  load5: number;
  load15: number;
  cores_total: number;
}

// 利用率与显存共用同一个 Go 类型（GPUStat），语义不通用两个字段区分。
// Available=false 时 Percent 为 null，并带 reason 供仪表写诊断。
export interface GpuStat {
  available: boolean;
  percent: number | null;
  used?: number;
  total?: number;
  name?: string;
  reason?: string;
  // 以下两项属于 M3（NVML）范围：后端现在还不返回，可选并在缺席时不显示。
  // 原型的 GPU 格确实要求「67°C 142W」，届时由 NVML 采集器补上。
  temp_c?: number;
  power_w?: number;
}

export interface Snapshot {
  seq: number;
  ts: number;
  warming: boolean;
  cpu: CpuStat | null;
  mem: MemStat | null;
  disks: DiskUsage[];
  gpu: GpuStat | null;
  // NVML 不可用时整体为 null（不是 available:false）。
  vram: GpuStat | null;
}