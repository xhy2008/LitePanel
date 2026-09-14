import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { flushPromises, mount } from '@vue/test-utils';
import { createPinia, setActivePinia } from 'pinia';
import { createRouter, createMemoryHistory } from 'vue-router';
import App from '../App.vue';
import { routes } from '../router';
import { setApi, resetApi } from '../api/inject';
import { createApi } from '../api/http';

// 五个标签页都必须能真正渲染进外壳（父路由无 component 后，
// 这一步最容易悄悄失效）。
async function visit(path: string) {
  const location = { pathname: '/', search: '', assign: vi.fn() };
  const fetchMock = vi.fn(async () =>
    new Response(JSON.stringify({ authenticated: true, must_change_password: false }), {
      headers: { 'Content-Type': 'application/json' },
    }),
  ) as unknown as typeof globalThis.fetch;
  setApi(createApi({ fetch: fetchMock, location }), location);

  const pinia = createPinia();
  setActivePinia(pinia);
  const router = createRouter({ history: createMemoryHistory(), routes });
  await router.push(path);
  await flushPromises();
  const w = mount(App, { global: { plugins: [router, pinia] } });
  await flushPromises();
  return { w, router };
}

beforeEach(() => setActivePinia(createPinia()));
afterEach(() => resetApi());

describe('五个标签页可渲染', () => {
  const cases: Array<[string, string]> = [
    ['/quick', '快捷命令'],
    ['/term', '终端'],
    ['/files', '文件管理'],
    ['/downloads', '下载'],
    ['/settings', '设置'],
  ];

  it.each(cases)('%s 渲染外壳与视图占位', async (path, title) => {
    const { w, router } = await visit(path);
    expect(router.currentRoute.value.path).toBe(path);
    // PC 宽度下外壳在场
    expect(w.find('.side-nav').exists()).toBe(true);
    expect(w.find('.content .view').exists()).toBe(true);
    expect(title.length).toBeGreaterThan(0);
  });
});
