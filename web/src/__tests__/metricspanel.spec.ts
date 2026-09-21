import { describe, expect, it } from 'vitest';
import { mount } from '@vue/test-utils';
import MetricsPanel from '../components/metrics/MetricsPanel.vue';
import type { Snapshot } from '../api/metrics';

// 右栏/登录页共用的实时仪表面板。
//
// 为什么要抽出来：PC 右栏和登录页要显示同一套指标，两处各写一遍
// 「4 个 Gauge 的 value/sub/unavailable 映射」必然漂移（GPU 的
// unavailable/reason 组合尤其绕）。规格取自 UI原型-电脑版-v3 的
// .ggrid/.gcell，不另发明布局。

const CIR = 2 * Math.PI * 33;
function dash(pct: number): string {
  return `${((CIR * pct) / 100).toFixed(1)} ${CIR.toFixed(1)}`;
}

function snap(over: Partial<Snapshot> = {}): Snapshot {
  return {
    seq: 1, ts: 1, warming: false,
    cpu: { percent: 42, cores_total: 16, load1: 1.5, load5: 1.2, load15: 1.0 },
    mem: {
      total: 8e9, available: 3e9, used: 5e9, percent: 62.5,
      swap_total: 0, swap_used: 0, swap_percent: 0,
    },
    disks: [
      { mountpoint: '/', device: '/dev/sda1', fstype: 'ext4', total: 1e11, used: 5e10, free: 5e10, percent: 50 },
    ],
    gpu: null, vram: null,
    ...over,
  };
}

describe('MetricsPanel', () => {
  it('渲染 4 个环形仪表（CPU/内存/GPU/显存）+ 磁盘区', () => {
    const w = mount(MetricsPanel, { props: { snapshot: snap() } });
    const names = w.findAll('.g-n').map((n) => n.text());
    expect(names).toEqual(['CPU', '内存', 'GPU', '显存']);
    expect(w.text()).toContain('磁盘');
  });

  it('数值与 dasharray 按快照绘制', () => {
    const w = mount(MetricsPanel, { props: { snapshot: snap() } });
    const fg = w.findAll('.g-fg');
    expect(fg[0].attributes('stroke-dasharray')).toBe(dash(42));
    expect(fg[1].attributes('stroke-dasharray')).toBe(dash(63)); // 62.5 → 四舍五入
  });

  it('中心读数 + 环外次要读数（CPU 负载、内存用量）', () => {
    const w = mount(MetricsPanel, { props: { snapshot: snap() } });
    const subs = w.findAll('.g-x').map((s) => s.text());
    expect(subs[0]).toContain('1.50');   // load1
    expect(subs[1]).toContain('5.0');
    expect(subs[1]).toContain('8.0');
  });

  it('快照为 null 时全部画空环，绝不显示 0%', () => {
    const w = mount(MetricsPanel, { props: { snapshot: null } });
    for (const el of w.findAll('.g-v')) expect(el.text()).toBe('--');
  });

  it('GPU 不可用时灰显并给出原因', () => {
    const w = mount(MetricsPanel, {
      props: { snapshot: snap({ gpu: { available: false, percent: null, reason: '未检测到 GPU 驱动' } as never }) },
    });
    expect(w.text()).toContain('未检测到 GPU 驱动');
  });

  it('磁盘交给 DiskBars 渲染', () => {
    const w = mount(MetricsPanel, { props: { snapshot: snap() } });
    expect(w.find('.disk-item').exists()).toBe(true);
  });
});