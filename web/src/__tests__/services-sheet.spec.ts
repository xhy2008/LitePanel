import { beforeEach, describe, expect, it, vi } from 'vitest';
import { flushPromises, mount } from '@vue/test-utils';
import { createPinia, setActivePinia } from 'pinia';
import { setApi, resetApi } from '../api/inject';
import LogSheet from '../components/services/LogSheet.vue';
import ServiceForm from '../components/services/ServiceForm.vue';
import type { ServiceRow } from '../api/services';

const location = { pathname: '/', search: '', assign: () => {} };

interface FakeApi {
  get: ReturnType<typeof vi.fn>;
  post: ReturnType<typeof vi.fn>;
  patch: ReturnType<typeof vi.fn>;
  put: ReturnType<typeof vi.fn>;
  del: ReturnType<typeof vi.fn>;
}

function fakeApi(log = { lines: ['a', 'b'], cached_lines: 2, buffer_limit: 500 }): FakeApi {
  return {
    get: vi.fn(async () => log),
    post: vi.fn(async () => ({})),
    patch: vi.fn(async () => ({})),
    put: vi.fn(async () => ({})),
    del: vi.fn(async () => ({ ok: true })),
  };
}

beforeEach(() => {
  setActivePinia(createPinia());
  resetApi();
});

describe('LogSheet', () => {
  it('打开即拉日志并渲染', async () => {
    const api = fakeApi();
    setApi(api as never, location);
    const w = mount(LogSheet, { props: { id: 7, name: 'frpc' } });
    await flushPromises();
    expect(api.get).toHaveBeenCalledWith('/api/services/7/log?tail=500');
    const box = w.find('.logbox').text();
    expect(box).toContain('a');
    expect(box).toContain('b');
  });

  // D19：日志只在内存环形缓冲里。这句话必须写在界面上 —— 否则用户以为
  // 这里能翻历史，翻不到时反而怀疑面板坏了。
  it('明示"内存缓存 · 不持久化"与行数上限', async () => {
    setApi(fakeApi() as never, location);
    const w = mount(LogSheet, { props: { id: 1, name: 'x' } });
    await flushPromises();
    expect(w.text()).toContain('不持久化');
    expect(w.text()).toContain('500');
  });

  // WS 追加必须受上限约束：无界数组能让跑一天的服务把浏览器标签页吃爆，
  // 而后端本来就只留 500 行，多留的都是自欺欺人。
  it('append 超上限后丢掉最旧的行', async () => {
    setApi(fakeApi() as never, location);
    const w = mount(LogSheet, { props: { id: 1, name: 'x', limit: 3 } });
    await flushPromises();
    (w.vm as unknown as { append: (l: string[]) => void }).append(['c', 'd']);
    await w.vm.$nextTick();
    const box = w.find('.logbox').text();
    expect(box).toContain('c');
    expect(box).toContain('d');
    expect(box).not.toContain('a');
  });

  it('清空调 DELETE 并清屏', async () => {
    const api = fakeApi();
    setApi(api as never, location);
    const w = mount(LogSheet, { props: { id: 4, name: 'x' } });
    await flushPromises();
    await w.find('.btn-clear').trigger('click');
    await flushPromises();
    expect(api.del).toHaveBeenCalledWith('/api/services/4/log');
    expect(w.find('.logbox').text()).toBe('');
  });

  it('关闭按钮发 close', async () => {
    setApi(fakeApi() as never, location);
    const w = mount(LogSheet, { props: { id: 1, name: 'x' } });
    await flushPromises();
    await w.find('.shx').trigger('click');
    expect(w.emitted('close')).toBeTruthy();
  });

  // 计划要求 PC 居中弹窗、手机底部上滑 sheet。只做一种的话，
  // 电脑上会拖着一条从屏幕底边伸上来的巨型抽屉，鼠标够不着顶部。
  it('centered 时换成居中弹窗形态', async () => {
    setApi(fakeApi() as never, location);
    const w = mount(LogSheet, { props: { id: 1, name: 'x', centered: true } });
    await flushPromises();
    expect(w.find('.sh').classes()).toContain('centered');
  });

  it('默认仍是底部抽屉', async () => {
    setApi(fakeApi() as never, location);
    const w = mount(LogSheet, { props: { id: 1, name: 'x' } });
    await flushPromises();
    expect(w.find('.sh').classes()).not.toContain('centered');
  });
});

describe('ServiceForm', () => {
  const base = {
    name: '', kind: 'command' as const, unit: '', start_cmd: '',
    stop_cmd: '', cwd: '', autostart: false, sort: 0,
  };

  // 校验放在发请求之前：让后端拒绝虽然结果一样，但用户要等一个往返
  // 才知道"名字没填"，而原因还被埋进一句 error 文案里。
  it('command 缺启动命令时不发请求', async () => {
    const api = fakeApi();
    setApi(api as never, location);
    const w = mount(ServiceForm, { props: { initial: { ...base, name: 'x' } } });
    await w.find('form').trigger('submit');
    await flushPromises();
    expect(api.post).not.toHaveBeenCalled();
    expect(w.text()).toContain('启动命令');
  });

  it('systemd 缺单元时不发请求', async () => {
    const api = fakeApi();
    setApi(api as never, location);
    const w = mount(ServiceForm, {
      props: { initial: { ...base, name: 'x', kind: 'systemd' as const } },
    });
    await w.find('form').trigger('submit');
    await flushPromises();
    expect(api.post).not.toHaveBeenCalled();
    expect(w.text()).toContain('单元');
  });

  it('名字为空时不发请求', async () => {
    const api = fakeApi();
    setApi(api as never, location);
    const w = mount(ServiceForm, { props: { initial: { ...base, start_cmd: 'ls' } } });
    await w.find('form').trigger('submit');
    await flushPromises();
    expect(api.post).not.toHaveBeenCalled();
  });

  // 提交体必须是 snake_case：后端 JSON 解码开了 DisallowUnknownFields，
  // 发驼峰会被直接 400。
  it('提交体是 snake_case', async () => {
    const api = fakeApi();
    setApi(api as never, location);
    const w = mount(ServiceForm, {
      props: { initial: { ...base, name: 'frpc', start_cmd: 'frpc -c f.toml', cwd: '/srv' } },
    });
    await w.find('form').trigger('submit');
    await flushPromises();
    expect(api.post).toHaveBeenCalledWith(
      '/api/services',
      expect.objectContaining({ name: 'frpc', start_cmd: 'frpc -c f.toml', cwd: '/srv' }),
    );
    const sent = api.post.mock.calls[0][1] as Record<string, unknown>;
    expect(sent).not.toHaveProperty('startCmd');
    expect(w.emitted('saved')).toBeTruthy();
  });

  it('编辑态预填并走 PATCH', async () => {
    const api = fakeApi();
    setApi(api as never, location);
    const row: ServiceRow = {
      id: 3, name: 'aria2', kind: 'command', unit: '', start_cmd: 'aria2c',
      stop_cmd: '', cwd: '/d', autostart: true, sort: 0, created_at: 0,
      state: 'stopped', pid: 0, started_at: 0,
    };
    const w = mount(ServiceForm, { props: { editing: row } });
    expect((w.find('input[name="name"]').element as HTMLInputElement).value).toBe('aria2');
    await w.find('form').trigger('submit');
    await flushPromises();
    expect(api.patch).toHaveBeenCalledWith(
      '/api/services/3',
      expect.objectContaining({ name: 'aria2' }),
    );
    expect(api.post).not.toHaveBeenCalled();
  });

  it('切到 systemd 时字段跟着换', async () => {
    setApi(fakeApi() as never, location);
    const w = mount(ServiceForm, { props: { initial: { ...base } } });
    expect(w.find('.inp[name="start_cmd"]').exists()).toBe(true);
    expect(w.find('input[name="unit"]').exists()).toBe(false);
    await w.find('select[name="kind"]').setValue('systemd');
    expect(w.find('input[name="unit"]').exists()).toBe(true);
    expect(w.find('.inp[name="start_cmd"]').exists()).toBe(false);
  });

  it('centered 透传到壳上（PC 编辑也该是居中弹窗）', async () => {
    setApi(fakeApi() as never, location);
    const w = mount(ServiceForm, {
      props: { initial: { name: '', kind: 'command', unit: '', start_cmd: '', stop_cmd: '', cwd: '', autostart: false, sort: 0 }, centered: true },
    });
    expect(w.find('.sh').classes()).toContain('centered');
  });

  it('取消发 close', async () => {
    setApi(fakeApi() as never, location);
    const w = mount(ServiceForm, { props: { initial: { ...base } } });
    await w.find('.btn-cancel').trigger('click');
    expect(w.emitted('close')).toBeTruthy();
  });
});
