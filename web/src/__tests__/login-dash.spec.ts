import { beforeEach, describe, expect, it } from 'vitest';
import { mount } from '@vue/test-utils';
import { createRouter, createMemoryHistory } from 'vue-router';
import { createPinia } from 'pinia';
import LoginView from '../views/LoginView.vue';
import { metricsState, resetMetrics } from '../composables/useMetrics';
import type { Snapshot } from '../api/metrics';

// 设计偏离（用户要求）：性能监控 API 公开，登录页也要能看到仪表盘。
// 布局（用户要求）：指标区固定在屏幕顶部，与 UI 原型的顶栏同规格，
// 登录卡片在其下方居中 —— 不是垫在卡片下面。
//
// 数据来源：App.vue 挂载时已统一 attachMetrics（HTTP 快照垫首帧 + WS 续帧），
// LoginView 只读共享单例 metricsState —— 不自建数据管道。

const snap: Snapshot = {
  seq: 1, ts: 1, warming: false,
  cpu: { percent: 42, cores_total: 4, load1: 0.5, load5: 0.4, load15: 0.3 },
  mem: {
    total: 1000, available: 500, used: 500, percent: 50,
    swap_total: 0, swap_used: 0, swap_percent: 0,
  },
  disks: [
    { mountpoint: '/', device: 'a', fstype: 'ext4', total: 1e11, used: 9e10, free: 1e10, percent: 90 },
  ],
  gpu: null, vram: null,
};

function setWidth(px: number) {
  Object.defineProperty(window, 'innerWidth', { value: px, configurable: true });
  window.dispatchEvent(new Event('resize'));
}

async function mountLogin() {
  const router = createRouter({
    history: createMemoryHistory(),
    routes: [
      { path: '/login', name: 'login', component: LoginView },
      { path: '/quick', name: 'quick', component: { template: '<div/>' } },
    ],
  });
  const w = mount(LoginView, {
    global: { plugins: [router, createPinia()] },
  });
  // 断点在 onMounted 里按 innerWidth 校正，要多等一次渲染。
  await w.vm.$nextTick();
  return w;
}

describe('登录页性能仪表盘', () => {
  beforeEach(() => {
    resetMetrics();
    // 断点是模块级单例，会跨用例残留，每个用例显式给宽度。
    setWidth(1400);
  });

  it('顶部指标条 + 登录卡片都在', async () => {
    metricsState.value.snapshot = snap;
    const w = await mountLogin();
    expect(w.find('input[name="password"]').exists()).toBe(true);
    expect(w.find('.login-topbar').exists()).toBe(true);
    expect(w.text()).toContain('42'); // CPU %
    expect(w.text()).toContain('内存');
    w.unmount();
  });

  // DOM 顺序即视觉顺序：指标条必须是第一个子元素，才谈得上"固定顶部"。
  it('指标条排在登录卡片之前（固定顶部，不是垫在下面）', async () => {
    metricsState.value.snapshot = snap;
    const w = await mountLogin();
    const html = w.html();
    expect(html.indexOf('login-topbar')).toBeLessThan(html.indexOf('class="card"'));
    w.unmount();
  });

  // 各断点都用同一条顶部竖条：紧凑、高度可控，且与手机版原型顶栏一致。
  it('手机与 PC 都用顶栏竖条', async () => {
    for (const width of [375, 1400]) {
      setWidth(width);
      metricsState.value.snapshot = snap;
      const w = await mountLogin();
      expect(w.find('.login-topbar .vbs').exists()).toBe(true);
      w.unmount();
    }
  });

  it('无快照时不崩，竖条画 --（设计的首帧空态）', async () => {
    const w = await mountLogin();
    expect(w.find('.login-topbar').exists()).toBe(true);
    const vs = w.findAll('.login-topbar .tick-v');
    expect(vs.length).toBeGreaterThan(0);
    for (const el of vs) expect(el.text()).toBe('--');
    w.unmount();
  });
});