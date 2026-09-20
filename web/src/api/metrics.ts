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
  cores: number;
  load1: number;
  load5: number;
  load15: number;
}

export interface GpuStat {
  available: boolean;
  percent: number;
  vram_used: number;
  vram_total: number;
  vram_percent: number;
  temp_c: number;
  power_w: number;
  reason: string;
}

export interface Snapshot {
  seq: number;
  ts: number;
  warming: boolean;
  cpu: CpuStat | null;
  mem: MemStat | null;
  disks: DiskUsage[];
  gpu: GpuStat | null;
  vram: GpuStat | null;
}