import { describe, expect, it } from 'vitest';
import { mount } from '@vue/test-utils';
import { createRouter, createMemoryHistory } from 'vue-router';
import AppShell from '../layout/AppShell.vue';
import { routes } from '../router';

// 断点分支渲染是 M1-T8 的核心验收（设计 16.1）：
// PC = 左 60px 图标条 + 右 128px 仪表轨；平板 = 无右轨；手机 = 顶部状态条 + 底部 tab。
async function mountAt(width: number) {
  Object.defineProperty(window, 'innerWidth', { value: width, configurable: true });
  window.dispatchEvent(new Event('resize'));

  const router = createRouter({ history: createMemoryHistory(), routes });
  await router.push('/quick');
  await router.isReady();

  return mount(AppShell, {
    global: {
      plugins: [router],
      stubs: { RouterView: { template: '<div class="router-stub" />' } },
    },
  });
}

describe('AppShell 断点分支', () => {
  it('PC：渲染图标导航条与右侧仪表轨', async () => {
    const w = await mountAt(1400);
    expect(w.find('.icon-rail').exists()).toBe(true);
    expect(w.find('.metrics-rail').exists()).toBe(true);
    expect(w.find('.phone-statusbar').exists()).toBe(false);
  });

  it('平板：有图标条，无右轨', async () => {
    const w = await mountAt(1000);
    expect(w.find('.icon-rail').exists()).toBe(true);
    expect(w.find('.metrics-rail').exists()).toBe(false);
    expect(w.find('.phone-statusbar').exists()).toBe(false);
  });

  it('手机：顶部状态条 + 底部导航，无左侧图标条', async () => {
    const w = await mountAt(400);
    expect(w.find('.phone-statusbar').exists()).toBe(true);
    expect(w.find('.phone-tabbar').exists()).toBe(true);
    expect(w.find('.icon-rail').exists()).toBe(false);
    expect(w.find('.metrics-rail').exists()).toBe(false);
  });

  it('手机顶栏必须是 6 根指标竖条（对齐 UI原型-手机版-v2）', async () => {
    const w = await mountAt(400);
    expect(w.findAll('.phone-statusbar .tick').length).toBe(6);
  });

  it('五个 tab 在任一形态都可通过导航点达', async () => {
    for (const width of [1400, 400]) {
      const w = await mountAt(width);
      const nav = width >= 1200 ? '.icon-rail' : '.phone-tabbar';
      expect(w.findAll(`${nav} .tab`).length).toBe(5);
    }
  });
});
