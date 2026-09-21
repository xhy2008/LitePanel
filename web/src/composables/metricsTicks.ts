import type { Snapshot } from '../api/metrics';

export interface TickBar {
  name: string;
  value?: number;
  color?: string;
}

// 手机顶栏 6 根竖条的映射：4 指标 + 分隔 + 占用率最高的盘。
// 规格取自 UI原型-手机版-v2 的 .vbs。
//
// 右栏外壳与登录页共用这一份：两处各写一遍必然漂移（GPU 的
// available/percent 组合、盘的告警色阈值尤其容易只改一边）。
export function tickBars(s: Snapshot | null): TickBar[] {
  const bars: TickBar[] = [
    { name: 'CPU', value: s?.cpu?.percent ?? undefined, color: '#FF6600' },
    { name: '内存', value: s?.mem?.percent ?? undefined, color: '#3ba7ff' },
    { name: 'GPU', value: s?.gpu?.available ? s.gpu.percent ?? undefined : undefined, color: '#2ecc71' },
    { name: '显存', value: s?.vram?.available ? s.vram.percent ?? undefined : undefined, color: '#f5a623' },
    // 占位：真正的分隔线由 MetricTicks 按索引 4 渲染。
    { name: 'sep', value: undefined },
  ];

  const disks = s?.disks ?? [];
  const top = disks.length > 0 ? disks.reduce((a, b) => (a.percent >= b.percent ? a : b)) : null;
  bars.push({
    name: top?.mountpoint ?? '--',
    value: top?.percent ?? undefined,
    color: top && top.percent > 90 ? 'var(--err)' : '#f5a623',
  });
  return bars;
}