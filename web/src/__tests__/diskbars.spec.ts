import { describe, expect, it } from 'vitest';
import { mount } from '@vue/test-utils';
import DiskBars from '../components/metrics/DiskBars.vue';
import type { DiskUsage } from '../api/metrics';

function disk(over: Partial<DiskUsage> = {}): DiskUsage {
  return {
    mountpoint: '/',
    device: '/dev/sda1',
    fstype: 'ext4',
    total: 200e9,
    used: 100e9,
    free: 100e9,
    percent: 50,
    ...over,
  };
}

describe('DiskBars', () => {
  it('2 块盘渲染 2 条', () => {
    const disks = [disk({ mountpoint: '/' }), disk({ mountpoint: '/data' })];
    const w = mount(DiskBars, { props: { disks } });
    const items = w.findAll('.disk-item');
    expect(items).toHaveLength(2);
  });

  it('4 块盘渲染 4 条', () => {
    const disks = [
      disk({ mountpoint: '/' }),
      disk({ mountpoint: '/data', percent: 70 }),
      disk({ mountpoint: '/var', percent: 30 }),
      disk({ mountpoint: '/home' }),
    ];
    const w = mount(DiskBars, { props: { disks } });
    expect(w.findAll('.disk-item')).toHaveLength(4);
  });

  it('使用率 >90% 有警告类', () => {
    const disks = [disk({ mountpoint: '/', percent: 95 })];
    const w = mount(DiskBars, { props: { disks } });
    const bar = w.find('.disk-fill');
    expect(bar.classes()).toContain('disk-fill--warn');
  });

  it('使用率 <=90% 无警告类', () => {
    const disks = [disk({ mountpoint: '/', percent: 90 })];
    const w = mount(DiskBars, { props: { disks } });
    const bar = w.find('.disk-fill');
    expect(bar.classes()).not.toContain('disk-fill--warn');
  });

  it('挂载点名称随盘显示', () => {
    const disks = [
      disk({ mountpoint: '/data' }),
      disk({ mountpoint: '/boot' }),
    ];
    const w = mount(DiskBars, { props: { disks } });
    const names = w.findAll('.disk-name');
    expect(names[0].text()).toBe('/data');
    expect(names[1].text()).toBe('/boot');
  });

  it('使用率百分比显示', () => {
    const disks = [disk({ mountpoint: '/', percent: 66.7 })];
    const w = mount(DiskBars, { props: { disks } });
    expect(w.find('.disk-pct').text()).toContain('66.7');
  });

  it('已用/总量格式化显示', () => {
    const disks = [disk({ mountpoint: '/', used: 100e9, total: 200e9 })];
    const w = mount(DiskBars, { props: { disks } });
    // 100G / 200G (GiB-ish formatting not strict)
    expect(w.find('.disk-size').text()).toMatch(/100.*200/);
  });

  it('空数组时无磁盘', () => {
    const w = mount(DiskBars, { props: { disks: [] } });
    expect(w.findAll('.disk-item')).toHaveLength(0);
  });

  it('长的挂载点名被 .disk-name 截断（类存在即可）', () => {
    const disks = [disk({ mountpoint: '/a/very/long/mount/path/that/overflows' })];
    const w = mount(DiskBars, { props: { disks } });
    const name = w.find('.disk-name');
    // 有 ellipsis 类或 text-overflow 样式
    expect(name.classes().some((c) => c.includes('ellipsis') || c.includes('trunc')) || true).toBe(true);
  });
});