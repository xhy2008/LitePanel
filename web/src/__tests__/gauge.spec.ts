import { describe, expect, it } from 'vitest';
import { mount } from '@vue/test-utils';
import Gauge from '../components/metrics/Gauge.vue';

// 环形仪表测试按设计 §10 与 v3 原型规格：
// - 环径 72px，描边 6px，内径 27px (36-6-3)，半径 r=33
// - 中心显示百分比 17px 粗体 + 名称 10px
// - 环外一行次要读数 8.5px
// - stroke-dasharray = pct × 2πr（约 207.35）
// - 不可用态 → 灰色 + 诊断文字

const CIR = 2 * Math.PI * 33; // ≈207.345

function dash(pct: number): string {
  const fill = CIR * pct / 100;
  return `${fill.toFixed(1)} ${CIR.toFixed(1)}`;
}

describe('Gauge', () => {
  it('渲染百分比与名称', () => {
    const w = mount(Gauge, { props: { name: 'CPU', value: 45 } });
    expect(w.text()).toContain('45');
    expect(w.text()).toContain('CPU');
    // 不包含 % 符号在角色协议里（.g-v 内含 <small>%</small>）
  });

  it('stroke-dasharray 与百分比成比例', () => {
    const w = mount(Gauge, { props: { name: 'CPU', value: 45 } });
    const fg = w.find('.g-fg');
    const da = fg.attributes('stroke-dasharray');
    expect(da).toBe(dash(45));
  });

  it('value=undefined 显示 --', () => {
    const w = mount(Gauge, { props: { name: 'CPU' } });
    expect(w.text()).toContain('--');
    expect(w.text()).toContain('CPU');
  });

  it('value=null 显示 --', () => {
    const w = mount(Gauge, { props: { name: '内存', value: null } });
    expect(w.text()).toContain('--');
  });

  it('不可用态显示灰色与诊断文字', () => {
    const w = mount(Gauge, {
      props: { name: 'GPU', unavailable: true, reason: '找不到 NVML' },
    });
    expect(w.text()).toContain('不可用');
    expect(w.text()).toContain('找不到 NVML');
    // 灰色态标记
    expect(w.find('.g').classes()).toContain('unavailable');
  });

  it('次要读数显示', () => {
    const w = mount(Gauge, {
      props: { name: '内存', value: 62, sub: '7.3/11.7G' },
    });
    expect(w.text()).toContain('7.3/11.7G');
  });

  it('100% 时占满整环', () => {
    const w = mount(Gauge, { props: { name: 'CPU', value: 100 } });
    const fg = w.find('.g-fg');
    // 100% 时 dasharray 第一段 = 周长，第二段 = 0（圆环满）
    expect(fg.attributes('stroke-dasharray')).toBe(dash(100));
  });

  it('0% 时完全不填充', () => {
    const w = mount(Gauge, { props: { name: 'CPU', value: 0 } });
    const fg = w.find('.g-fg');
    expect(fg.attributes('stroke-dasharray')).toBe(dash(0));
  });

  it('负数被截为 0', () => {
    const w = mount(Gauge, { props: { name: 'CPU', value: -5 } });
    const fg = w.find('.g-fg');
    expect(fg.attributes('stroke-dasharray')).toBe(dash(0));
  });

  it('超 100 被截为 100', () => {
    const w = mount(Gauge, { props: { name: 'CPU', value: 150 } });
    const fg = w.find('.g-fg');
    expect(fg.attributes('stroke-dasharray')).toBe(dash(100));
  });
});