import type { JobProgress } from '../api/fsJobs';

export interface JobStreamDeps {
  store: { applyProgress: (p: JobProgress) => void; load?: () => Promise<unknown> };
  wsClient: { subscribe: (ch: string, h: (d: unknown) => void) => () => void };
}

/**
 * 把 fsjobs 频道接进任务 store。
 *
 * 与 useServiceStream 同一形状（单独成函数、替身注入 ws），理由也一样：
 * 接线本身只有三行，埋在组件里就只能靠起整个 App 才测得到，而它一旦断线
 * 的症状是"抽屉永远停在旧数字" —— 任务在后台跑得好好的，编译和后端测试
 * 全都不会红，只有人盯着界面才发现。
 */
export function attachJobStream(deps: JobStreamDeps): () => void {
  return deps.wsClient.subscribe('fsjobs', (data) => {
    deps.store.applyProgress(data as JobProgress);
  });
}
