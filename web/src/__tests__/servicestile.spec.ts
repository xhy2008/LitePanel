import { describe, expect, it, vi } from 'vitest';
import { mount } from '@vue/test-utils';
import ServiceTile from '../components/services/ServiceTile.vue';
import AppIcon from '../components/AppIcon.vue';
import { hasIcon } from '../components/iconPaths';
import { describeService } from '../stores/services';
import type { ServiceRow } from '../api/services';

function row(over: Partial<ServiceRow> = {}): ServiceRow {
  return {
    id: 1, name: 'llama-server', kind: 'command', unit: '',
    start_cmd: 'llama-server', stop_cmd: '', cwd: '', autostart: false,
    sort: 0, created_at: 0, state: 'running', pid: 41287,
    started_at: Math.floor(Date.now() / 1000) - 7200, ...over,
  };
}

// 状态文案的唯一来源是 describeService（D21）。视图里不许自己拼字符串：
// 一旦两处写法漂移，排查线上问题时读到的就是完全错误的含义。
describe('ServiceTile', () => {
  it('运行中：PID + 状态色 + 开关在 on', () => {
    const w = mount(ServiceTile, { props: { row: row() } });
    expect(w.text()).toContain('llama-server');
    expect(w.text()).toContain('PID 41287');
    expect(w.find('.st-run').exists()).toBe(true);
    expect(w.find('.sw.on').exists()).toBe(true);
  });

  it('异常退出：红色 + code', () => {
    const w = mount(ServiceTile, {
      props: { row: row({ state: 'stopped', pid: 0, exit_reason: 'error', exit_code: 1 }) },
    });
    expect(w.find('.st-bad').exists()).toBe(true);
    expect(w.text()).toContain('异常退出');
    expect(w.text()).toContain('code 1');
    expect(w.find('.sw.on').exists()).toBe(false);
  });

  it('未启动：中性色，不假装退出过', () => {
    const w = mount(ServiceTile, { props: { row: row({ state: 'stopped', pid: 0 }) } });
    expect(w.text()).toContain('未启动');
    expect(w.text()).not.toContain('code 0');
  });

  it('CMD / SYSV 徽标', () => {
    expect(mount(ServiceTile, { props: { row: row() } }).text()).toContain('CMD');
    expect(
      mount(ServiceTile, { props: { row: row({ kind: 'systemd', unit: 'nginx' }) } }).text(),
    ).toContain('SYSV');
  });

  // 请求在飞时开关必须禁用：连点不是 UI 瑕疵，是真的会把服务反复启停。
  it('busy 时开关禁用且不再发 toggle', async () => {
    const w = mount(ServiceTile, {
      props: { row: row({ state: 'stopping' }), busy: true },
    });
    expect(w.find('.sw').attributes('aria-disabled')).toBe('true');
    await w.find('.sw').trigger('click');
    expect(w.emitted('toggle')).toBeUndefined();
    expect(w.find('.st-run').text()).toContain('停止中');
  });

  // 磁贴不整体可点：只有信息区打开编辑，开关和日志各管各的。
  it('点信息区发 edit，不误发 toggle/log', async () => {
    const w = mount(ServiceTile, { props: { row: row() } });
    await w.find('.tinfo').trigger('click');
    expect(w.emitted('edit')).toEqual([[1]]);
    expect(w.emitted('toggle')).toBeUndefined();
    expect(w.emitted('log')).toBeUndefined();
  });

  it('点开关发 toggle，点日志按钮发 log，互不干扰', async () => {
    const w = mount(ServiceTile, { props: { row: row() } });
    await w.find('.sw').trigger('click');
    expect(w.emitted('toggle')).toEqual([[1]]);
    expect(w.emitted('log')).toBeUndefined();

    const w2 = mount(ServiceTile, { props: { row: row() } });
    await w2.find('.ib').trigger('click');
    expect(w2.emitted('log')).toEqual([[1]]);
    expect(w2.emitted('toggle')).toBeUndefined();
  });

  // 描述文案与 store 的判定必须是同一份实现，这里钉住这个契约。
  it('展示的文案就是 describeService 的输出', () => {
    const r = row({ state: 'stopped', pid: 0, exit_reason: 'clean', exit_code: 0, exit_at: Math.floor(Date.now() / 1000) });
    const w = mount(ServiceTile, { props: { row: r } });
    expect(w.find('.st').text()).toContain(describeService(r).text.replace(/ · .*/, ''));
  });
});

// AppIcon 对未知名字是 `PATHS[name] ?? []` —— 静默画一个空 SVG，
// 于是"图标没了"这种退化在页面上几乎看不出来。
// 名字不是抄在这里的名单，而是从 describeService 的实际输出里取：
// 硬编码名单只会保护"测试自己写的那几个字"，实现换成任意错名字照过。
describe('describeService 用到的图标都必须有图形', () => {
  const states: Array<Partial<ServiceRow>> = [
    { state: 'running' },
    { state: 'starting' },
    { state: 'stopping' },
    { state: 'stopped' },
    { state: 'stopped', exit_reason: 'clean', exit_code: 0 },
    { state: 'stopped', exit_reason: 'error', exit_code: 1 },
    { state: 'running', kind: 'systemd', unit: 'nginx' },
  ];
  const names = states.map((o) => describeService(row(o)).icon);

  it.each([...new Set(names)])('%s', (name) => {
    expect(hasIcon(name), `图标 ${name} 缺路径定义`).toBe(true);
    const w = mount(AppIcon, { props: { name } });
    expect(w.find('path').exists()).toBe(true);
  });

  // 名单不该是空的 —— 空名单会让上面整组测试空转。
  it('状态枚举确实产出了图标名', () => {
    expect(new Set(names).size).toBeGreaterThanOrEqual(4);
  });
});
