import { describe, it, expect, beforeEach, afterEach, vi } from 'vitest';
import { mount, type VueWrapper } from '@vue/test-utils';
import { setActivePinia, createPinia } from 'pinia';
import { setApi, resetApi } from '../api/inject';
import UploadDropZone from '../components/files/UploadDropZone.vue';
import { useFilesStore } from '../stores/files';
import { useUploadsStore } from '../stores/uploads';

// 拖放区看着只是"拖进来→入队"，真正要盯的是三条浏览器行为：
//   1. 掉在列表外面时不能被浏览器"直接打开"（会跳标签页，像面板坏了）；
//   2. 拖过子元素时高亮不能闪烁；
//   3. 组件反复挂载不能积累 window 监听器。

function dragData(files: File[] = [], types: string[] = ['Files']) {
  return { dataTransfer: { files, types } };
}

function file(name: string): File {
  return { name, size: 100, lastModified: 1700000000000, slice: () => new Blob() } as unknown as File;
}

// 每个用例结束时把组件（以及它挂在 window 上的监听器）拆干净。
// 不拆的话，前一个用例留下的监听器会让"卸载后不再拦截"那条永远失败 ——
// 而失败原因看起来像实现有 bug。
let live: VueWrapper[] = [];

function mk() {
  const w = mount(UploadDropZone, { props: { dir: '/data' } });
  live.push(w);
  return w;
}

beforeEach(() => {
  setActivePinia(createPinia());
  resetApi();
  useFilesStore().dir = '/data';
  setApi(
    { get: vi.fn(), post: vi.fn(), del: vi.fn(), patch: vi.fn(), put: vi.fn() } as never,
    { pathname: '/files', search: '', assign: () => {} },
    vi.fn() as never,
  );
});

afterEach(() => {
  for (const w of live) w.unmount();
  live = [];
});

describe('UploadDropZone', () => {
  it('拖入文件后入队，目标目录是当前目录', async () => {
    const w = mk();
    await w.trigger('drop', dragData([file('a.txt'), file('b.txt')]));
    const u = useUploadsStore();
    expect(u.items.map((i) => i.name)).toEqual(['a.txt', 'b.txt']);
    expect(u.items[0].dir).toBe('/data');
  });

  it('dragenter 显示高亮，drop 后收起', async () => {
    const w = mk();
    expect(w.find('.hint').exists()).toBe(false);
    await w.trigger('dragenter', dragData());
    expect(w.find('.hint').exists()).toBe(true);
    await w.trigger('drop', dragData());
    expect(w.find('.hint').exists()).toBe(false);
  });

  // 拖过子元素会成对触发一堆 enter/leave。用计数而不是"leave 就关",
  // 否则高亮会疯狂闪烁。
  it('一次进入里的多对 enter/leave 不会提前收起高亮', async () => {
    const w = mk();
    await w.trigger('dragenter', dragData());
    await w.trigger('dragenter', dragData());
    await w.trigger('dragleave', dragData());
    expect(w.find('.hint').exists()).toBe(true);
    await w.trigger('dragleave', dragData());
    expect(w.find('.hint').exists()).toBe(false);
  });

  // 掉在列表外面时若不吞掉默认行为，浏览器会直接打开这个文件：
  // 图片跳到新标签页、二进制跳到下载页，上传根本没开始。
  it('window 上的 drop / dragover 被吞掉，防止浏览器接管文件', async () => {
    mk();
    await Promise.resolve();
    const drop: any = new Event('drop', { bubbles: true, cancelable: true });
    window.dispatchEvent(drop);
    expect(drop.defaultPrevented).toBe(true);
    const over: any = new Event('dragover', { bubbles: true, cancelable: true });
    window.dispatchEvent(over);
    expect(over.defaultPrevented).toBe(true);
  });

  it('卸载后拆掉 window 监听器', async () => {
    const w = mk();
    await Promise.resolve();
    w.unmount();
    const drop: any = new Event('drop', { bubbles: true, cancelable: true });
    window.dispatchEvent(drop);
    expect(drop.defaultPrevented).toBe(false);
  });

  // 拖进来的是选中文字或图片链接时，高亮成"松手上传到 /data"是句假话:
  // 那里根本没有文件可传，松手只会什么都不发生。
  it('非文件拖拽不高亮', async () => {
    const w = mk();
    await w.trigger('dragenter', { dataTransfer: { files: [], types: ['text/plain'] } });
    expect(w.find('.hint').exists()).toBe(false);
  });

  it('松手时什么都没带就不入队', async () => {
    const w = mk();
    await w.trigger('drop', dragData([]));
    expect(useUploadsStore().items).toEqual([]);
  });
});
