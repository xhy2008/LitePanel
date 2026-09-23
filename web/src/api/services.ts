// 与后端 internal/api/handlers_services.go 的 serviceView 一一对应。
// 后端是 snake_case（Go struct tag），这里保持同名：HTTP 与 WS 序列化的是
// 同一个类型，前端一旦自己改名就会与其中一条悄悄漂移。

export type ServiceKind = 'command' | 'systemd';
export type ServiceState = 'stopped' | 'starting' | 'running' | 'stopping';
export type ExitReason = 'clean' | 'error';

export interface ServiceRow {
  id: number;
  name: string;
  kind: ServiceKind;
  unit: string;
  start_cmd: string;
  stop_cmd: string;
  cwd: string;
  autostart: boolean;
  sort: number;
  created_at: number;
  state: ServiceState;
  pid: number;
  started_at: number;
  // exit_reason 缺席（undefined）= 从没退出过。
  // 不能把缺席当成 code 0：那会把"一次都没启动"显示成"跑过且正常退出"。
  exit_reason?: ExitReason;
  exit_code?: number;
  exit_signal?: number;
  exit_at?: number;
  stopped_by?: string;
}

// WS services 频道的事件体（后端 service.Event）。
export interface ServiceEvent {
  id: number;
  state: ServiceState;
  // reload 事件（新建/删除）只带 id/state/reload，不带 pid。
  pid?: number;
  exit?: {
    code: number;
    signal: number;
    reason: ExitReason;
    at: number;
    stopped_by: string;
  };
  // 新建/删除时后端只说"列表变了"，前端只能重拉。
  reload?: boolean;
}

export interface ServiceLog {
  lines: string[];
  cached_lines: number;
  buffer_limit: number;
}

export interface ServiceInput {
  name: string;
  kind: ServiceKind;
  unit?: string;
  start_cmd?: string;
  stop_cmd?: string;
  cwd?: string;
  autostart?: boolean;
  sort?: number;
}
