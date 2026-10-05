import { beforeEach, describe, expect, it, vi } from 'vitest';
import { flushPromises, mount } from '@vue/test-utils';
import { createPinia, setActivePinia } from 'pinia';
import DownloadsView from '../views/DownloadsView.vue';
import { setApi, resetApi } from '../api/inject';
import type { DlTask } from '../api/downloads';

function task(over: Partial<DlTask> = {}): DlTask {
  return {
    gid: 'g1',
    uris: ['https://example.com/files/a.zip'],
    state: 'active',
    total_bytes: 1000,
    done_bytes: 250,
    speed: 2048,
    connections: 4,
    created_at: 1,
    can_control: true,
    ...over,
  };
}

let tasks: DlTask[] = [];
let health: { ok: boolean; message?: string } = { ok: true };
let addBody: unknown = null;

function boot() {
  setActivePinia(createPinia());
  addBody = null;
  const api = {
    get: vi.fn(async (p: string) => {
      if (p === '/api/dl/tasks') return { tasks };
      if (p === '/api/dl/summary') {
        return { speed: 2048, active: 1, waiting: 0, paused: 0, done: 0, failed: 0 };
      }
      return health;
    }),
    post: vi.fn(async (_p: string, body?: unknown) => {
      addBody = body;
      return { ok: true };
    }),
    del: vi.fn(async () => ({ ok: true, cleared: 1 })),
    patch: vi.fn(async () => ({})),
    put: vi.fn(async () => ({})),
  };
  setApi(api as never, { pathname: '/', search: '', assign() {} } as never);
  return api;
}

// wsClient 替身：记录订阅的频道，并能手动推一条事件。
function fakeWs() {
  const handlers: ((d: unknown) => void)[] = [];
  const chs: string[] = [];
  return {
    chs,
    emit: (d: unknown) => handlers.forEach((h) => h(d)),
    client: {
      subscribe: (ch: string, h: (d: unknown) => void) => {
        chs.push(ch);
        handlers.push(h);
        return () => {};
      },
    },
  };
}

async function render(ws?: unknown) {
  const w = mount(DownloadsView, {
    global: { plugins: [createPinia()] },
    props: ws ? ({ wsClient: ws } as never) : {},
  });
  await flushPromises();
  return w;
}

beforeEach(() => {
  resetApi();
  tasks = [];
  health = { ok: true };
});

describe('DownloadsView', () => {
  it('aria2 掉线出横幅且带后端原因', async () => {
    boot();
    health = { ok: false, message: 'connection refused' };
    const w = await render();
    expect(w.find('.dl-alert').text()).toContain('connection refused');
  });

  it('活动任务渲染进度百分比、速度与文件名', async () => {
    boot();
    tasks = [task()];
    const w = await render();
    const txt = w.text();
    expect(txt).toContain('a.zip');
    expect(txt).toContain('25%');
    expect(txt).toContain('2.0 KB/s');
  });

  it('失败任务展开后显示 aria2 的错误原文', async () => {
    boot();
    tasks = [task({ gid: 'f', state: 'error', error: 'File not found' })];
    const w = await render();
    await w.find('.dl-fail-h').trigger('click');
    expect(w.text()).toContain('File not found');
  });

  it('can_control=false 的行按钮禁用', async () => {
    boot();
    tasks = [task({ state: 'paused', can_control: false })];
    const w = await render();
    const btns = w.findAll('.dl-acts button');
    expect(btns.length).toBeGreaterThan(0);
    for (const b of btns) expect(b.attributes('disabled')).toBeDefined();
  });

  it('新建下载：多行地址拆成 uris 数组提交', async () => {
    const api = boot();
    const w = await render();
    await w.find('.dl-top .dl-btn.primary').trigger('click');
    await w.find('textarea').setValue('https://a/1.zip\nhttps://b/1.zip\n\n');
    await w.find('.dl-f input').setValue('/data/down');
    // 提交按钮是对话框里最后一个 .primary
    await w.findAll('.dl-btn.primary').at(-1)!.trigger('click');
    await flushPromises();
    expect(addBody).toEqual({
      uris: ['https://a/1.zip', 'https://b/1.zip'],
      dir: '/data/down',
      out: undefined,
    });
    void api;
  });

  it('订阅了 downloads 频道', async () => {
    boot();
    const ws = fakeWs();
    await render(ws.client);
    expect(ws.chs).toContain('downloads');
  });

  it('downloads 事件触发列表重拉', async () => {
    const api = boot();
    tasks = [task()];
    const ws = fakeWs();
    await render(ws.client);
    const before = api.get.mock.calls.filter((c) => c[0] === '/api/dl/tasks').length;
    ws.emit({ kind: 'completed', gid: 'g1', at: 1 });
    await new Promise((r) => setTimeout(r, 350));
    const after = api.get.mock.calls.filter((c) => c[0] === '/api/dl/tasks').length;
    expect(after).toBeGreaterThan(before);
  });

  it('空列表显示引导文案而不是空白', async () => {
    boot();
    const w = await render();
    expect(w.find('.dl-hint').text()).toContain('新建下载');
  });

  it('magnet 任务显示占位标题而不是整条链接', async () => {
    boot();
    tasks = [
      task({
        gid: 'm',
        uris: ['magnet:?xt=urn:btih:' + 'a'.repeat(40)],
        name: undefined,
      }),
    ];
    const w = await render();
    expect(w.find('.dl-name').text()).toContain('BT');
    expect(w.find('.dl-name').text()).not.toContain('magnet:?');
  });
});
