import { describe, it, expect, beforeEach, vi } from 'vitest';
import { mount } from '@vue/test-utils';
import { setActivePinia, createPinia } from 'pinia';
import { setApi, resetApi } from '../api/inject';
import TrashPanel from '../components/files/TrashPanel.vue';
import { useFilesStore } from '../stores/files';
import type { TrashItem } from '../api/files';

// 回收站面板的逻辑重点：还原/删掉之后列表要重取（服务端才是真相）,
// 而失败时要把后端的原话显示出来 —— "恢复失败"四个字会让人以为回收站
// 坏了，真正的原因往往是目标位置有个同名文件挡着。

function item(over: Partial<TrashItem> = {}): TrashItem {
  return {
    id: 't1', name: 'backup.tar', is_dir: false, size: 1024,
    origin: '/data/backup.tar', mount: '/data', deleted_at: Math.floor(Date.now() / 1000),
    path: '/data/.litepanel-trash/t1', ...over,
  };
}

const push = vi.fn();
vi.mock('vue-router', () => ({ useRouter: () => ({ push }) }));

function mk(items: TrashItem[]) {
  const get = vi.fn(async () => ({ items }));
  const post = vi.fn(async (url: string) => {
    if (url.includes('/restore')) return {};
    if (url.includes('/empty')) return { removed: items.length };
    return {};
  });
  const del = vi.fn(async (_url: string) => ({}));
  resetApi();
  setApi({ get, post, del, patch: vi.fn(), put: vi.fn() } as never,
    { pathname: '/files', search: '', assign: () => {} }, vi.fn() as never);
  const w = mount(TrashPanel, { global: { stubs: { AppIcon: true } } });
  return { w, get, post, del };
}

beforeEach(() => {
  setActivePinia(createPinia());
  push.mockReset();
});

// 挂载即拉取：抽屉打开时列表不该是空的再闪一下。
it('挂载时读取回收站列表', async () => {
  const { w, get } = mk([item()]);
  await flush();
  expect(get).toHaveBeenCalledWith('/api/fs/trash');
  expect(w.text()).toContain('backup.tar');
});

it('空回收站给出明确文案（不是空白一块）', async () => {
  const { w } = mk([]);
  await flush();
  expect(w.text()).toContain('回收站是空的');
});

it('每项显示原位置，而不是回收站里的乱码路径', async () => {
  const { w } = mk([item()]);
  await flush();
  expect(w.text()).toContain('/data/backup.tar');
  expect(w.text()).not.toContain('.litepanel-trash');
});

// 还原成功后重取列表（而不是本地 splice）：还原在服务端还改了原目录,
// 列表里少了什么、多了什么只有后端知道。
it('还原调用接口并重取列表', async () => {
  const { w, post, get } = mk([item()]);
  await flush();
  get.mockResolvedValue({ items: [] } as never);
  await w.findAll('.ti')[0].findAll('.b')[1].trigger('click');
  await flush();
  expect(post).toHaveBeenCalledWith('/api/fs/trash/t1/restore');
  expect(get).toHaveBeenCalledTimes(2);
});

it('还原失败时把后端文案原样显示出来', async () => {
  const { w, post } = mk([item()]);
  await flush();
  post.mockRejectedValueOnce(new Error('目标位置已存在同名文件'));
  await w.findAll('.ti')[0].findAll('.b')[1].trigger('click');
  await flush();
  expect(w.find('.err').text()).toContain('目标位置已存在同名文件');
});

it('删掉只移除这一项，且带 confirm=1（服务端强制二次确认）', async () => {
  const { w, del } = mk([item(), item({ id: 't2', name: 'x.txt' })]);
  await flush();
  await w.findAll('.ti')[0].findAll('.b')[2].trigger('click');
  await flush();
  expect(del.mock.calls[0][0]).toContain('/api/fs/trash/t1?confirm=1');
  expect(w.findAll('.ti')).toHaveLength(1);
});

// 清空是不可逆的整盘操作：点一下只进入"待确认"，第二下才发请求。
it('清空需要两步确认', async () => {
  const { w, post } = mk([item()]);
  await flush();
  const btns = w.findAll('.ft .b');
  await btns[1].trigger('click');
  expect(post).not.toHaveBeenCalledWith(expect.stringContaining('empty'));
  const again = w.findAll('.ft .b');
  await again[again.length - 1].trigger('click');
  await flush();
  expect(post.mock.calls.some((c) => String(c[0]).includes('/trash/empty?confirm=1'))).toBe(true);
});

it('空回收站时没有清空按钮', async () => {
  const { w } = mk([]);
  await flush();
  expect(w.findAll('.ft .b')).toHaveLength(1);
});

// 恢复失败多半是同名冲突；跳到原目录让用户看见那个挡路的文件。
it('查看位置跳到原所在目录并关闭面板', async () => {
  const { w } = mk([item()]);
  await flush();
  const store = useFilesStore();
  const spy = vi.spyOn(store, 'open').mockResolvedValue(undefined);
  await w.findAll('.ti')[0].findAll('.b')[0].trigger('click');
  expect(spy).toHaveBeenCalledWith('/data');
  expect(w.emitted('close')).toBeTruthy();
});

async function flush() {
  for (let i = 0; i < 4; i++) await Promise.resolve();
}
