import { reactive } from 'vue';
import type { Snapshot } from '../api/metrics';

export interface MetricsState {
  snapshot: Snapshot | null;
  connected: boolean;
  cleanups: (() => void)[];
}

export const metricsState: { value: MetricsState } = {
  value: reactive<MetricsState>({ snapshot: null, connected: false, cleanups: [] }),
};

export function resetMetrics() {
  metricsState.value.snapshot = null;
  metricsState.value.connected = false;
  metricsState.value.cleanups.forEach((fn) => fn());
  metricsState.value.cleanups = [];
}

export interface AttachDeps {
  fetchSnap: () => Promise<Snapshot>;
  wsClient: {
    subscribe: (ch: string, h: (data: unknown, seq: number) => void) => () => void;
    onStatus: (h: (s: string) => void) => () => void;
  };
}

/**
 * 挂载实时指标数据管道，用于仪表盘和右栏。
 *
 * 1. 消掉前次挂载的残留（单例只有一条活跃）
 * 2. HTTP 快照垫首屏（不等 WS 握手）
 * 3. WS metrics 频道接收后续帧
 * 4. 绑定 WS 连接状态 → connected
 * 5. 返回 stop() 用于卸载
 */
export async function attachMetrics(deps: AttachDeps): Promise<() => void> {
  // 清理前次残留（多次 attachMetrics 只记最后一条）
  const prev = metricsState.value.cleanups;
  prev.forEach((fn) => fn());
  prev.length = 0;

  const cleanups: (() => void)[] = [];
  // 覆写全局 cleanups 引用，让后续 attachMetrics 能清理本次
  metricsState.value.cleanups = cleanups;

  // 1. HTTP 快照首屏
  const snap = await deps.fetchSnap();
  metricsState.value.snapshot = snap;
  metricsState.value.connected = true;

  // 2. WS 帧更新
  const unsubMetrics = deps.wsClient.subscribe('metrics', (data, _seq) => {
    metricsState.value.snapshot = data as Snapshot;
  });
  cleanups.push(unsubMetrics);

  // 3. WS 连接态
  const unsubStatus = deps.wsClient.onStatus((s) => {
    metricsState.value.connected = s === 'online';
  });
  cleanups.push(unsubStatus);

  // 返回 stop：只做清理，不重置状态值（测试依赖连通性快照在 stop 后仍然可读）
  return () => {
    cleanups.forEach((fn) => fn());
    cleanups.length = 0;
    // 把全局数组也清掉，避免重复清理
    if (metricsState.value.cleanups === cleanups) {
      metricsState.value.cleanups = [];
    }
  };
}