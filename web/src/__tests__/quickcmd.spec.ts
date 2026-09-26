import { describe, expect, it, vi } from 'vitest';
import { flushPromises, mount } from '@vue/test-utils';
import { createPinia, setActivePinia } from 'pinia';
import { setApi, resetApi } from '../api/inject';
import QuickCmdView from '../views/QuickCmdView.vue';
import { makeRouter } from './cmdpanelHarness';

const location = { pathname: '/quick', search: '', assign: () => {} };

function fakeApi() {
  return {
    get: vi.fn(async (path: string) =>
      path === '/api/services' ? { services: [] } : { commands: [] },
    ),
    post: vi.fn(async () => ({})),
    patch: vi.fn(async () => ({})),
    put: vi.fn(async () => ({})),
    del: vi.fn(async () => ({ ok: true })),
  };
}

// 视图在没有 wsClient prop 时会自己取浏览器默认实例，而那需要真实
// WebSocket。替身返回可用客户端，顺便记下订阅的频道。
const seen: string[] = [];
vi.mock('../api/ws', () => ({
  tryWs: () => ({
    subscribe: (ch: string) => {
      seen.push(ch);
      return () => {};
    },
    onStatus: () => () => {},
  }),
}));

describe('QuickCmdView 分段', () => {
  it('默认显示服务管理分段', async () => {
    setActivePinia(createPinia());
    resetApi();
    const api = fakeApi();
    setApi(api as never, location);
    const w = mount(QuickCmdView);
    await flushPromises();
    expect(api.get).toHaveBeenCalledWith('/api/services');
    expect(w.find('.svc').exists()).toBe(true);
    // 默认分段就得自己订上状态频道，否则页面打开后要靠别处才会刷新。
    expect(seen).toContain('services');
  });

  // 分段标题就是原型里的「服务管理 / 快捷命令」，顺序也不能换：
  // 服务是这一页的主用途，把命令排前面会让用户找不到托管入口。
  it('两个分段可切换', async () => {
    setActivePinia(createPinia());
    resetApi();
    setApi(fakeApi() as never, location);
    const w = mount(QuickCmdView);
    await flushPromises();
    const segs = w.findAll('.segb');
    expect(segs.map((s) => s.text())).toEqual(['服务管理', '快捷命令']);
    expect(segs[0].classes()).toContain('on');
    await segs[1].trigger('click');
    expect(w.findAll('.segb')[1].classes()).toContain('on');
  });

  // 快捷命令分段不再是一句"将来落地"的占位：M5 之后它就是真列表。
  // 占位文案留在页面上是最坏的一种退化 —— 页面看着正常，功能却整块没了。
  it('切到快捷命令分段后渲染真列表，不是占位文案', async () => {
    setActivePinia(createPinia());
    resetApi();
    setApi(fakeApi() as never, location);
    const w = mount(QuickCmdView, { global: { plugins: [makeRouter()] } });
    await flushPromises();
    await w.findAll('.segb')[1].trigger('click');
    await flushPromises();
    expect(w.text()).not.toContain('将在 M5 落地');
    expect(w.find('.cmds').exists()).toBe(true);
    expect(w.find('.tile-add').exists()).toBe(true);
  });

  // v-if 而不是 v-show：切走就该停掉那一侧的列表请求与订阅。
  it('切回服务管理时分段整块卸载', async () => {
    setActivePinia(createPinia());
    resetApi();
    setApi(fakeApi() as never, location);
    const w = mount(QuickCmdView, { global: { plugins: [makeRouter()] } });
    await flushPromises();
    await w.findAll('.segb')[1].trigger('click');
    await flushPromises();
    await w.findAll('.segb')[0].trigger('click');
    expect(w.find('.cmds').exists()).toBe(false);
  });
});
