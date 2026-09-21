import { beforeEach, describe, expect, it } from 'vitest';
import { mount } from '@vue/test-utils';
import { createRouter, createMemoryHistory } from 'vue-router';
import { createPinia } from 'pinia';
import LoginView from '../views/LoginView.vue';
import { metricsState, resetMetrics } from '../composables/useMetrics';
import type { Snapshot } from '../api/metrics';

// 设计偏离（用户要求）：性能监控 API 公开，登录页也要能看到仪表盘。
// 登录页不再是一张光秃秃的密码卡 —— 卡片旁实时显示机器状态，
// 用户在输密码前就能瞄一眼这台服务器忙不忙。
//
// 数据来源：App.vue 挂载时统一 attachMetrics（HTTP 快照垫首帧 + WS 续帧），
// LoginView 只读共享单例 metricsState —— 不自建数据管道。

const snap: Snapshot = {
  seq: 1, ts: 1, warming: false,
  cpu: { percent: 42, cores_total: 4, load1: 0.5, load5: 0.4, load15: 0.3 },
  mem: {
    total: 1000, available: 500, used: 500, percent: 50,
    swap_total: 0, swap_used: 0, swap_percent: 0,
  },
  disks: [],
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

  it('PC：登录表单旁渲染实时仪表栏', async () => {
    metricsState.value.snapshot = snap;
    const w = await mountLogin();
    expect(w.find('input[name="password"]').exists()).toBe(true);
    expect(w.find('.login-dash').exists()).toBe(true);
    expect(w.text()).toContain('42'); // CPU %
    expect(w.text()).toContain('内存');
    w.unmount();
  });

  // 断点分支与主外壳一致（设计 16.1）：手机端不给右栏，给顶栏那种竖条。
  it('手机：登录页用竖条而不是仪表栏', async () => {
    setWidth(375);
    metricsState.value.snapshot = snap;
    const w = await mountLogin();
    expect(w.find('.vbs').exists()).toBe(true);
    expect(w.find('.g').exists()).toBe(false);
    w.unmount();
  });

  it('无快照时不崩，环上画 --（设计的首帧空态）', async () => {
    const w = await mountLogin();
    expect(w.find('.login-dash').exists()).toBe(true);
    // 名字照旧显示，数值一律 --，绝不拿 0 当真实值
    const vs = w.findAll('.g-v');
    expect(vs.length).toBeGreaterThan(0);
    for (const el of vs) expect(el.text()).toBe('--');
    w.unmount();
  });
});