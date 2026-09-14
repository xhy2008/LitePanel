import { describe, expect, it } from 'vitest';
import { flushPromises, mount } from '@vue/test-utils';
import { createRouter, createMemoryHistory, type RouteRecordRaw } from 'vue-router';
import { createPinia } from 'pinia';
import App from '../App.vue';

// 断点分支渲染是 M1-T8 的核心验收（设计 16.1）：
//   ≥1200  PC   ：左 60px 图标条 + 中栏 + 右 128px 常驻仪表轨
//   768–1199 平板：左栏维持图标条，右栏改为可拉出的抽屉
//   <768   手机  ：顶栏一行 = 左侧「当前页图标」下拉按钮 + 右侧 6 根竖直条；无独立指标区
const Blank = { template: '<div class="view" />' };
const routes: RouteRecordRaw[] = [
  { path: '/', redirect: '/quick' },
  { path: '/quick', name: 'quick', component: Blank, meta: { title: '快捷命令' } },
  { path: '/term', name: 'term', component: Blank, meta: { title: '终端' } },
  { path: '/files', name: 'files', component: Blank, meta: { title: '文件管理' } },
  { path: '/downloads', name: 'downloads', component: Blank, meta: { title: '下载' } },
  { path: '/settings', name: 'settings', component: Blank, meta: { title: '设置' } },
  { path: '/login', name: 'login', component: Blank, meta: { title: '登录', bare: true } },
];

async function mountApp(width: number, path = '/quick') {
  Object.defineProperty(window, 'innerWidth', { value: width, configurable: true });
  window.dispatchEvent(new Event('resize'));

  const router = createRouter({ history: createMemoryHistory(), routes });
  await router.push(path);
  await router.isReady();

  const w = mount(App, {
    global: { plugins: [router, createPinia()] },
    attachTo: document.body,
  });
  return { w, router };
}

describe('App 三栏骨架的断点分支', () => {
  it('PC：图标条 + 内容 + 常驻仪表轨三栏并排', async () => {
    const { w } = await mountApp(1400);
    expect(w.find('.side-nav').exists()).toBe(true);
    expect(w.find('.right-rail').exists()).toBe(true);
    expect(w.find('.mobile-tabmenu').exists()).toBe(false);
  });

  it('平板：保留图标条，右栏退成抽屉（默认收起，可拉出）', async () => {
    const { w } = await mountApp(1000);
    expect(w.find('.side-nav').exists()).toBe(true);
    const rail = w.find('.right-rail');
    expect(rail.exists()).toBe(true);
    expect(rail.classes()).toContain('drawer');
    expect(rail.classes()).not.toContain('open');

    await w.get('.rail-drawer-toggle').trigger('click');
    expect(w.get('.right-rail').classes()).toContain('open');
  });

  it('手机：顶栏含下拉按钮与 6 根竖直条，无左栏无右栏', async () => {
    const { w } = await mountApp(375);
    expect(w.find('.mobile-topbar').exists()).toBe(true);
    expect(w.find('.mobile-tabmenu').exists()).toBe(true);
    expect(w.findAll('.mobile-topbar .tick').length).toBe(6);
    expect(w.find('.side-nav').exists()).toBe(false);
    expect(w.find('.right-rail').exists()).toBe(false);
  });

  it('手机顶栏的下拉按钮图标随当前页变化', async () => {
    const { w, router } = await mountApp(375, '/term');
    const before = w.get('.mobile-tabmenu .trigger').html();
    await router.push('/settings');
    await w.vm.$nextTick();
    const after = w.get('.mobile-tabmenu .trigger').html();
    expect(before).not.toBe(after);
  });

  it('手机下拉展开后能切到五个标签页', async () => {
    const { w, router } = await mountApp(375);
    await w.get('.mobile-tabmenu .trigger').trigger('click');
    const items = w.findAll('.mobile-tabmenu .menu .item');
    expect(items.length).toBe(5);
    await items[3].trigger('click');
    await flushPromises(); // router.push 是异步的
    expect(router.currentRoute.value.name).toBe('downloads');
  });

  it('PC 图标条是纯图标：5 个按钮，带中文 title 提示，不带可见文字', async () => {
    const { w } = await mountApp(1400);
    const tabs = w.findAll('.side-nav .tab');
    expect(tabs.length).toBe(5);
    for (const t of tabs) {
      expect(t.attributes('title')).toBeTruthy();
      expect(t.text()).toBe('');
    }
  });

  it('登录页是裸页：不渲染导航与仪表轨', async () => {
    const { w } = await mountApp(1400, '/login');
    expect(w.find('.side-nav').exists()).toBe(false);
    expect(w.find('.right-rail').exists()).toBe(false);
  });

  it('点击 PC 图标条可切页', async () => {
    const { w, router } = await mountApp(1400);
    await w.findAll('.side-nav .tab')[2].trigger('click');
    await flushPromises();
    expect(router.currentRoute.value.name).toBe('files');
  });
});
