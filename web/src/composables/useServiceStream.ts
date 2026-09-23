import type { ServiceEvent } from '../api/services';

export interface StreamDeps {
  store: { applyEvent: (ev: ServiceEvent) => void };
  wsClient: { subscribe: (ch: string, h: (d: unknown) => void) => () => void };
}

/**
 * 把 services 频道的 WS 事件接进 store。
 *
 * 单独成函数（而不是埋在组件里）是为了能替身注入 ws 客户端做测试 ——
 * 事件落不到 store 的话，界面上会一直挂着绿色的 "PID 41287"，
 * 而进程早就没了。返回退订函数给 onUnmounted。
 */
export function attachServiceStream(deps: StreamDeps): () => void {
  return deps.wsClient.subscribe('services', (data) => {
    deps.store.applyEvent(data as ServiceEvent);
  });
}
