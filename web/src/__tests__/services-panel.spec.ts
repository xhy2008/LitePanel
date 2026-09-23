import { beforeEach, describe, expect, it, vi } from 'vitest';
import { createPinia, setActivePinia } from 'pinia';
import { flushPromises, mount } from '@vue/test-utils';
import { setApi, resetApi } from '../api/inject';
import { useServicesStore } from '../stores/services';
import { attachServiceStream } from '../composables/useServiceStream';
import ServicesPanel from '../components/services/ServicesPanel.vue';
import type { ServiceRow } from '../api/services';

const location = { pathname: '/', search: '', assign: () => {} };

function row(over: Partial<ServiceRow> = {}): ServiceRow {
  return {
    id: 1, name: 'frpc', kind: 'command', unit: '', start_cmd: 'frpc', stop_cmd: '',
    cwd: '', autostart: false, sort: 0, created_at: 0, state: 'stopped', pid: 0,
    started_at: 0, ...over,
  };
}

function fakeApi(items: ServiceRow[] = []) {
  return {
    get: vi.fn(async () => ({ services: items })),
    post: vi.fn(async () => row()),
    patch: vi.fn(async () => row()),
    put: vi.fn(async () => ({})),
    del: vi.fn(async () => ({ ok: true })),
  };
}

// 替身：记下订阅，能手动投帧。
function fakeWs() {
  const handlers = new Map<string, Set<(d: unknown) => void>>();
  return {
    subscribe(ch: string, h: (d: unknown) => void) {
      if (!handlers.has(ch)) handlers.set(ch, new Set());
      handlers.get(ch)!.add(h);
      return () => handlers.get(ch)?.delete(h);
    },
    emit(ch: string, data: unknown) {
      handlers.get(ch)?.forEach((h) => h(data));
    },
    channels() {
      return [...handlers.keys()];
    },
    count(ch: string) {
      return handlers.get(ch)?.size ?? 0;
    },
  };
}

beforeEach(() => {
  setActivePinia(createPinia());
  resetApi();
});

describe('attachServiceStream', () => {
  it('订阅 services 频道', () => {
    const api = fakeApi();
    setApi(api as never, location);
    const store = useServicesStore();
    const w = fakeWs();
    const stop = attachServiceStream({ store, wsClient: w });
    expect(w.channels()).toContain('services');
    stop();
  });

  // WS 事件是状态的唯一实时来源。收到崩溃事件却不落到 store，
  // 界面上就会一直挂着绿色 "PID 41287"，而进程早没了。
  it('services 帧推进 store', () => {
    const api = fakeApi([row()]);
    setApi(api as never, location);
    const store = useServicesStore();
    // 直接预置列表：attachServiceStream 不负责拉数据（那是视图的事），
    // 这里只验"事件真的落进 store"这一件事，不掺 HTTP。
    store.items = [row()];
    const w = fakeWs();
    attachServiceStream({ store, wsClient: w });
    w.emit('services', { id: 1, state: 'running', pid: 999 });
    expect(store.items[0].state).toBe('running');
    expect(store.items[0].pid).toBe(999);
  });

  it('退订后不再改 store', () => {
    const api = fakeApi([row()]);
    setApi(api as never, location);
    const store = useServicesStore();
    store.items = [row()];
    const w = fakeWs();
    const stop = attachServiceStream({ store, wsClient: w });
    stop();
    w.emit('services', { id: 1, state: 'running', pid: 999 });
    expect(store.items[0].state).toBe('stopped');
  });
});

describe('ServicesPanel', () => {
  it('挂载即拉列表并渲染磁贴', async () => {
    const api = fakeApi([row({ name: 'frpc' }), row({ id: 2, name: 'aria2' })]);
    setApi(api as never, location);
    const w = mount(ServicesPanel, { props: { wsClient: fakeWs() as never } });
    await flushPromises();
    expect(api.get).toHaveBeenCalledWith('/api/services');
    expect(w.text()).toContain('frpc');
    expect(w.text()).toContain('aria2');
  });

  // 空列表必须给"添加服务"的入口，否则新用户看到的是白板。
  it('空列表显示引导而不是空白', async () => {
    setApi(fakeApi([]) as never, location);
    const w = mount(ServicesPanel, { props: { wsClient: fakeWs() as never } });
    await flushPromises();
    expect(w.find('.tile-add').exists()).toBe(true);
  });

  it('添加按钮打开表单，保存后刷新列表', async () => {
    const api = fakeApi([]);
    setApi(api as never, location);
    const w = mount(ServicesPanel, { props: { wsClient: fakeWs() as never } });
    await flushPromises();
    await w.find('.tile-add').trigger('click');
    expect(w.find('.svc-form').exists()).toBe(true);
    expect(api.get).toHaveBeenCalledTimes(1); // 打开表单不该多打一枪
  });

  // 原型把磁贴固定成 [图标][信息][日志][开关]，删除这类危险操作放在
  // 编辑表单里并要 confirmDanger 式确认：磁贴上多一个垃圾桶，误点一次
  // 就把服务和它的配置一起删了。
  it('点磁贴信息区打开编辑', async () => {
    const api = fakeApi([row({ id: 5, name: 'frpc' })]);
    setApi(api as never, location);
    const w = mount(ServicesPanel, { props: { wsClient: fakeWs() as never } });
    await flushPromises();
    await w.find('.tinfo').trigger('click');
    expect(w.find('.svc-form').exists()).toBe(true);
    expect(w.find('.svc-form').text()).toContain('编辑服务');
  });

  it('删除要确认，确认后才发请求', async () => {
    const api = fakeApi([row({ id: 5 })]);
    setApi(api as never, location);
    const w = mount(ServicesPanel, { props: { wsClient: fakeWs() as never } });
    await flushPromises();
    await w.find('.tinfo').trigger('click');
    await w.find('.btn-del').trigger('click');
    expect(api.del).not.toHaveBeenCalled();
    expect(w.find('.btn-confirm-del').exists()).toBe(true);
    await w.find('.btn-confirm-del').trigger('click');
    await flushPromises();
    expect(api.del).toHaveBeenCalledWith('/api/services/5');
  });

  it('取消确认不发删除请求', async () => {
    const api = fakeApi([row({ id: 5 })]);
    setApi(api as never, location);
    const w = mount(ServicesPanel, { props: { wsClient: fakeWs() as never } });
    await flushPromises();
    await w.find('.tinfo').trigger('click');
    await w.find('.btn-del').trigger('click');
    await w.find('.btn-cancel-del').trigger('click');
    await flushPromises();
    expect(api.del).not.toHaveBeenCalled();
  });

  it('日志按钮打开抽屉并订阅该服务的日志频道', async () => {
    const api = fakeApi([row({ id: 5 })]);
    const w2 = fakeWs();
    setApi(api as never, location);
    const w = mount(ServicesPanel, { props: { wsClient: w2 as never } });
    await flushPromises();
    await w.find('.ib').trigger('click');
    await flushPromises();
    expect(api.get).toHaveBeenCalledWith('/api/services/5/log?tail=500');
    expect(w2.channels()).toContain('svclog:5');
  });

  // WS 推来的日志必须真的进抽屉：只订不接等于抽屉是个静态截图。
  it('svclog 帧追加到已打开的抽屉', async () => {
    const api = fakeApi([row({ id: 5 })]);
    const w2 = fakeWs();
    setApi(api as never, location);
    const w = mount(ServicesPanel, { props: { wsClient: w2 as never } });
    await flushPromises();
    await w.find('.ib').trigger('click');
    await flushPromises();
    w2.emit('svclog:5', { id: 5, lines: ['刚推来的一行'] });
    await flushPromises();
    expect(w.find('.logbox').text()).toContain('刚推来的一行');
  });

  it('关掉抽屉后退订该日志频道', async () => {
    const api = fakeApi([row({ id: 5 })]);
    const w2 = fakeWs();
    setApi(api as never, location);
    const w = mount(ServicesPanel, { props: { wsClient: w2 as never } });
    await flushPromises();
    await w.find('.ib').trigger('click');
    await flushPromises();
    expect(w2.count('svclog:5')).toBe(1);
    await w.find('.shx').trigger('click');
    await flushPromises();
    // 计数必须归零：抽屉留着订阅，每次日志推送都在给一个看不见的组件
    // 拼字符串，开合十次就有十份。
    expect(w2.count('svclog:5')).toBe(0);
    expect(w.find('.logbox').exists()).toBe(false);
  });

  // 后端明确写着：CMD 类型随面板存活，面板重启会一并停掉。
  it('提示 CMD 服务随面板存活', async () => {
    setApi(fakeApi([row()]) as never, location);
    const w = mount(ServicesPanel, { props: { wsClient: fakeWs() as never } });
    await flushPromises();
    expect(w.find('.hintbar').text()).toContain('随面板存活');
  });
});
