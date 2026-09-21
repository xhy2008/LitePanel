import { describe, expect, it } from 'vitest';
import { tickBars } from '../composables/metricsTicks';
import type { Snapshot } from '../api/metrics';

// 手机顶栏 6 根竖条的映射，右栏外壳与登录页共用一份。
// 两处各写一遍必然漂移（原型手机版 v2：CPU/内存/GPU/显存 + 分隔 + 占用最高的盘）。

function snap(over: Partial<Snapshot> = {}): Snapshot {
  return {
    seq: 1, ts: 1, warming: false,
    cpu: { percent: 10, cores_total: 4, load1: 0, load5: 0, load15: 0 },
    mem: { total: 1, available: 1, used: 0, percent: 20, swap_total: 0, swap_used: 0, swap_percent: 0 },
    disks: [
      { mountpoint: '/', device: 'a', fstype: 'ext4', total: 1, used: 0.4, free: 0.6, percent: 40 },
      { mountpoint: '/data', device: 'b', fstype: 'ext4', total: 1, used: 0.9, free: 0.1, percent: 90 },
    ],
    gpu: null, vram: null,
    ...over,
  };
}

describe('tickBars', () => {
  it('固定 6 格：4 指标 + 分隔 + 一个盘', () => {
    const bars = tickBars(snap());
    expect(bars).toHaveLength(6);
    expect(bars.map((b) => b.name)).toEqual(['CPU', '内存', 'GPU', '显存', 'sep', '/data']);
  });

  it('第 5 格是分隔符且无值', () => {
    const bars = tickBars(snap());
    expect(bars[4].value).toBeUndefined();
  });

  it('磁盘取占用率最高的那块', () => {
    expect(tickBars(snap())[5].name).toBe('/data');
  });

  it('占用 >90% 的盘用告警色', () => {
    const hot = snap();
    hot.disks[1].percent = 95;
    expect(tickBars(hot)[5].color).toBe('var(--err)');
  });

  it('无快照时全格无值（画 --），名称仍在', () => {
    const bars = tickBars(null);
    expect(bars).toHaveLength(6);
    for (const b of bars.slice(0, 4)) expect(b.value).toBeUndefined();
    expect(bars[5].name).toBe('--');
  });

  it('GPU 不可用时该格无值而不是 0', () => {
    const s = snap({ gpu: { available: false, percent: null, reason: 'no nvml' } });
    expect(tickBars(s)[2].value).toBeUndefined();
  });

  it('显存取 vram.percent（与 GPU 同一个 Go 类型）', () => {
    const s = snap({ vram: { available: true, percent: 77, used: 8e9, total: 11e9 } });
    expect(tickBars(s)[3].value).toBe(77);
  });
});