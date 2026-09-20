import { beforeEach, describe, expect, it, vi } from 'vitest';
import { attachMetrics, metricsState, resetMetrics, type MetricsState } from '../composables/useMetrics';
import type { Snapshot } from '../api/metrics';

// attachMetrics 的接线契约：
//  1. 启动即发一次 HTTP 快照 —— 首屏不能等 WS 握手（OnDemand 就是为它存在的）
//  2. 之后订阅 metrics 频道，WS 帧更新状态
//  3. 频道断开（offline）→ 数据标记失联；恢复在线后由 WS 客户端自动重订阅
//  4. 返回的 stop() 必须真正退订，不留全局定时器或监听器

function makeFeed(snap: unknown = { warming: false, seq: 0, ts: 0, cpu: null, mem: null, disks: [], gpu: null, vram: null }) {
  type Handler = (data: any, seq: number) => void;
  type StatusHandler = (s: string) => void;
  const handlers = new Set<Handler>();
  const statusHandlers = new Set<StatusHandler>();
  const client = {
    subscribe: vi.fn((_ch: string, h: Handler) => {
      handlers.add(h);
      return () => handlers.delete(h);
    }),
    onStatus: vi.fn((h: StatusHandler) => {
      statusHandlers.add(h);
      return () => statusHandlers.delete(h);
    }),
  };
  const fetchSnap = vi.fn(async () => snap as Snapshot);
  return { handlers, fetchSnap, client: client as any, statusHandlers };
}

function baseSnap(over: Record<string, unknown> = {}): Snapshot {
  return {
    seq: 1,
    ts: 1730000000000,
    warming: false,
    cpu: { percent: 45.7, cores: 4, load1: 0.5, load5: 0.4, load15: 0.1 },
    mem: {
      total: 12e9,
      available: 4e9,
      used: 8e9,
      percent: 66.7,
      swap_total: 2e9,
      swap_used: 0,
      swap_percent: 0,
    },
    disks: [
      { mountpoint: '/', device: '/dev/sda1', fstype: 'ext4', total: 200e9, used: 100e9, free: 100e9, percent: 50 },
    ],
    gpu: { available: false, percent: 0, vram_used: 0, vram_total: 0, vram_percent: 0, temp_c: 0, power_w: 0, reason: '找不到 libnvidia-ml.so.1' },
    vram: null,
    ...over,
  } as any;
}

describe('attachMetrics', () => {
  beforeEach(() => {
    resetMetrics();
  });

  it('启动后立即发起一次 HTTP 快照', async () => {
    const { fetchSnap, client } = makeFeed();
    const stop = await attachMetrics({ fetchSnap, wsClient: client as any });
    expect(fetchSnap).toHaveBeenCalledTimes(1);
    stop();
  });

  it('HTTP 快照后状态被正确填入', async () => {
    const s = baseSnap();
    const { fetchSnap, client } = makeFeed(s);
    const stop = await attachMetrics({ fetchSnap, wsClient: client as any });
    // await tick — attachMetrics is async, but fetchSnap is awaited inside
    // Wait for microtasks (the async resolution chain inside attachMetrics)
    await vi.waitFor(() => {
      expect(metricsState.value.snapshot).not.toBeNull();
    });
    expect(metricsState.value.snapshot!.cpu!.percent).toBe(45.7);
    expect(metricsState.value.snapshot!.warming).toBe(false);
    expect(metricsState.value.connected).toBe(true);
    stop();
  });

  it('warming=true → 用户层 percent 为 null（显示 --）', async () => {
    const s = baseSnap({ warming: true, cpu: { percent: null, cores: 4, load1: 0, load5: 0, load15: 0 } });
    const { fetchSnap, client } = makeFeed(s);
    const stop = await attachMetrics({ fetchSnap, wsClient: client as any });
    await vi.waitFor(() => {
      expect(metricsState.value.snapshot).not.toBeNull();
    });
    expect(metricsState.value.snapshot!.cpu!.percent).toBeNull();
    expect(metricsState.value.snapshot!.warming).toBe(true);
    stop();
  });

  it('CPU 块为 null → 降级为空（不是 0%）', async () => {
    const s = baseSnap({ cpu: null });
    const { fetchSnap, client } = makeFeed(s);
    const stop = await attachMetrics({ fetchSnap, wsClient: client as any });
    await vi.waitFor(() => {
      expect(metricsState.value.snapshot).not.toBeNull();
    });
    expect(metricsState.value.snapshot!.cpu).toBeNull();
    stop();
  });

  it('订阅 metrics 频道', async () => {
    const { fetchSnap, client } = makeFeed();
    const stop = await attachMetrics({ fetchSnap, wsClient: client as any });
    expect(client.subscribe).toHaveBeenCalledWith('metrics', expect.any(Function));
    stop();
  });

  it('WS 帧到来后更新状态', async () => {
    const { fetchSnap, client, handlers } = makeFeed();
    const stop = await attachMetrics({ fetchSnap, wsClient: client as any });
    await vi.waitFor(() => {
      expect(metricsState.value.snapshot).not.toBeNull();
    });

    const s = baseSnap({ seq: 2, cpu: { percent: 62.1, cores: 4, load1: 0.7, load5: 0.5, load15: 0.2 } });
    // 触发 WS handler
    for (const h of handlers) h(s, s.seq);

    expect(metricsState.value.snapshot!.cpu!.percent).toBe(62.1);
    expect(metricsState.value.snapshot!.seq).toBe(2);
    stop();
  });

  it('WS 断开 → connected=false', async () => {
    const { fetchSnap, client, statusHandlers } = makeFeed();
    const stop = await attachMetrics({ fetchSnap, wsClient: client as any });
    await vi.waitFor(() => {
      expect(metricsState.value.connected).toBe(true);
    });

    // 模拟 WS 离线
    for (const h of statusHandlers) h('offline');
    expect(metricsState.value.connected).toBe(false);
    stop();
  });

  it('WS 重连 → connected=true', async () => {
    const { fetchSnap, client, statusHandlers } = makeFeed();
    const stop = await attachMetrics({ fetchSnap, wsClient: client as any });
    await vi.waitFor(() => {
      expect(metricsState.value.connected).toBe(true);
    });

    for (const h of statusHandlers) h('offline');
    for (const h of statusHandlers) h('online');

    expect(metricsState.value.connected).toBe(true);
    stop();
  });

  it('stop() 后不再处理 WS 帧', async () => {
    const { fetchSnap, client, handlers } = makeFeed();
    const stop = await attachMetrics({ fetchSnap, wsClient: client as any });
    await vi.waitFor(() => {
      expect(metricsState.value.snapshot).not.toBeNull();
    });

    const prevSeq = metricsState.value.snapshot!.seq;
    stop();

    const s = baseSnap({ seq: 999 });
    for (const h of handlers) h(s, s.seq);

    expect(metricsState.value.snapshot!.seq).toBe(prevSeq);
  });

  it('stop() 后不再监听 WS 状态', async () => {
    const { fetchSnap, client, statusHandlers } = makeFeed();
    const stop = await attachMetrics({ fetchSnap, wsClient: client as any });
    await vi.waitFor(() => {
      expect(metricsState.value.connected).toBe(true);
    });

    stop();

    // offline 应该没有效果
    for (const h of statusHandlers) h('offline');
    // 但 connected 还是 true — 因为 stop() 移除了回调
    // 不过 connected 可能从未变过；这里验证没有新回调触发导致变为 false
    await vi.waitFor(() => {
      expect(metricsState.value.connected).toBe(true);
    });
  });

  it('多次 attachMetrics 只记录最后一条', async () => {
    const f1 = makeFeed();
    const f2 = makeFeed();
    const stop1 = await attachMetrics({ fetchSnap: f1.fetchSnap, wsClient: f1.client as any });
    const stop2 = await attachMetrics({ fetchSnap: f2.fetchSnap, wsClient: f2.client as any });

    // 发送 WS 帧到第一条（应已被忽略）
    const s = baseSnap({ seq: 42 });
    for (const h of f1.handlers) h(s, s.seq);
    expect(metricsState.value.snapshot!.seq).toBe(0); // never updated

    // 发送 WS 帧到第二条
    for (const h of f2.handlers) h(s, s.seq);
    expect(metricsState.value.snapshot!.seq).toBe(42);

    stop1();
    stop2();
  });
});