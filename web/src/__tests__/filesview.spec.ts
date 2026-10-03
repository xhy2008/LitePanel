import { beforeEach, describe, expect, it, vi } from 'vitest';
import { flushPromises, mount } from '@vue/test-utils';
import { createPinia, setActivePinia } from 'pinia';
import { createRouter, createMemoryHistory } from 'vue-router';
import FilesView from '../views/FilesView.vue';
import FileRow from '../components/files/FileRow.vue';
import { setApi, resetApi } from '../api/inject';
import { useFilesStore } from '../stores/files';
import type { FsEntry, ListPage } from '../api/files';

// 文件页是这一轮里唯一把 store、菜单、上传、任务四套东西接到一起的地方。
// 测试盯的不是样式，而是"点下去到底打了哪个请求"—— 界面写错一次请求
// 参数，后面全是错，而且错在真机上要跑几步才看得出来。

function entry(name: string, over: Partial<FsEntry> = {}): FsEntry {
  return {
    name, is_dir: name === 'sub', is_symlink: false, size: 10,
    mtime: 1700000000, mime: 'text/plain', mode: '-rw-r--r--', ...over,
  };
}

function page(over: Partial<ListPage> = {}): ListPage {
  return { path: '/data', page: 1, size: 500, total: 2, entries: [entry('a.txt'), entry('sub')], ...over };
}

const get = vi.fn();
const post = vi.fn();
const del = vi.fn();

async function mk(listPage: ListPage = page(), opts: { url?: string; roots?: string[] } = {}) {
  get.mockImplementation(async (url: string) => {
    if (url.startsWith('/api/fs/roots')) return { roots: (opts.roots ?? ['/data']).map((p) => ({ path: p, device: 'x', fstype: 'ext4', total: 1, free: 1, used: 0, measured: true })) };
    if (url.startsWith('/api/fs/list')) return listPage;
    if (url.startsWith('/api/fs/trash')) return { items: [] };
    return {};
  });
  post.mockResolvedValue({ job_id: 7, id: 7, state: 'pending' });
  del.mockResolvedValue({});
  resetApi();
  setApi({ get, post, del, patch: vi.fn(), put: vi.fn() } as never,
    { pathname: '/files', search: '', assign: () => {} }, vi.fn() as never);

  const pinia = createPinia();
  setActivePinia(pinia);
  const router = createRouter({
    history: createMemoryHistory(),
    routes: [
      { path: '/', component: FilesView },
      { path: '/term', name: 'term', component: { template: '<div/>' } },
    ],
  });
  await router.push(opts.url ?? '/');
  const w = mount(FilesView, { global: { plugins: [router, pinia], stubs: { AppIcon: true } } });
  await flushPromises();
  return { w, router };
}

beforeEach(() => {
  get.mockReset();
  post.mockReset();
  del.mockReset();
});

describe('FilesView 加载', () => {
  it('挂载即拉列表与挂载点', async () => {
    const { w } = await mk();
    expect(get.mock.calls.some((c) => String(c[0]).startsWith('/api/fs/list?'))).toBe(true);
    expect(get).toHaveBeenCalledWith('/api/fs/roots');
    expect(w.findAllComponents(FileRow)).toHaveLength(2);
  });

  // 参数必须完整：少了 sort/hidden 就等于前端悄悄换了后端的默认排序。
  it('列表请求带上分页、排序与隐藏文件开关', async () => {
    await mk();
    const q = String(get.mock.calls.find((c) => String(c[0]).startsWith('/api/fs/list'))![0]);
    // 挂载时还没有任何目录，首屏落在根：后端会把 path 回显成解析后的真实路径。
    for (const p of ['path=%2F', 'page=1', 'size=500', 'sort=name']) {
      expect(q).toContain(p);
    }
  });

  // 后端读的是 order=desc / show_hidden=1 —— **省略即默认值**。
  // 断言这些确切拼法是因为前后端各写一套时，参数名错了不会报错,
  // 只会让"排序按钮点了没反应"这种没有错误信息的现象出现。
  it('切换隐藏文件会带着 show_hidden=1 重新查询', async () => {
    const { w } = await mk();
    get.mockClear();
    await w.findAll('.tools2 .tb')[1].trigger('click');
    await flushPromises();
    expect(String(get.mock.calls[0][0])).toContain('show_hidden=1');
  });

  it('再点一次降序会带 order=desc', async () => {
    const { w } = await mk();
    get.mockClear();
    await w.findAll('.tools2 .tb')[0].trigger('click');
    await flushPromises();
    expect(String(get.mock.calls[0][0])).toContain('order=desc');
  });

  it('换排序字段会带着新 sort 重新查询', async () => {
    const { w } = await mk();
    get.mockClear();
    await w.find('.sb').setValue('mtime');
    await flushPromises();
    expect(String(get.mock.calls[0][0])).toContain('sort=mtime');
  });

  it('点挂载点 chip 跳到那个盘', async () => {
    const { w } = await mk(page({ path: '/mnt/disk1' }), { roots: ['/data', '/mnt/disk1'] });
    get.mockClear();
    await w.findAll('.rb')[1].trigger('click');
    await flushPromises();
    expect(String(get.mock.calls[0][0])).toContain('path=%2Fmnt%2Fdisk1');
  });

  it('后端报错时把原话显示出来（不能只说"失败"）', async () => {
    const { w } = await mk();
    // mk() 自己设过 get，所以这里换实现必须在 mk 之后。
    get.mockRejectedValueOnce(Object.assign(new Error('没有权限访问该目录'), { code: 'forbidden' }));
    await useFilesStore().open('/nope');
    await flushPromises();
    expect(w.find('.err').text()).toContain('没有权限访问该目录');
  });

  it('空目录给出明确文案', async () => {
    const { w } = await mk(page({ entries: [], total: 0 }));
    expect(w.find('.muted').text()).toContain('空目录');
  });
});

describe('FilesView 浏览', () => {
  it('点目录进入它', async () => {
    const { w } = await mk();
    get.mockClear();
    await w.findAllComponents(FileRow)[1].find('.row').trigger('click');
    await flushPromises();
    expect(String(get.mock.calls[0][0])).toContain('path=%2Fdata%2Fsub');
  });

  // 手机端"点文件即打开"会让多选变成不可能（想勾第二个必须先返回）。
  it('点文件是选中，不是打开', async () => {
    const { w } = await mk();
    const store = useFilesStore();
    await w.findAllComponents(FileRow)[0].find('.row').trigger('click');
    expect(store.selected).toEqual(['a.txt']);
  });

  it('点面包屑某一级跳到那一级', async () => {
    const { w } = await mk(page({ path: '/data/x/y' }));
    get.mockClear();
    await w.findAll('.seg')[1].trigger('click');
    await flushPromises();
    expect(String(get.mock.calls[0][0])).toContain('path=%2Fdata');
  });

  it('地址栏输入路径后回车跳转', async () => {
    const { w } = await mk();
    await w.find('.bar .txt').trigger('click');
    await w.find('.bar input').setValue('/var/log');
    const ev: any = new Event('keydown', { bubbles: true, cancelable: true });
    ev.key = 'Enter';
    w.find('.bar input').element.dispatchEvent(ev);
    await flushPromises();
    expect(String(get.mock.calls.at(-1)![0])).toContain('path=%2Fvar%2Flog');
  });

  // 滚到底必须追下一页，否则 10 万文件的目录只看得到前 500 个,
  // 而界面看起来就是"这个目录只有 500 个文件"。
  it('滚到底部追下一页', async () => {
    const { w } = await mk(page({ total: 5000 }));
    get.mockClear();
    const el = w.find('.list').element as HTMLElement;
    Object.defineProperty(el, 'scrollHeight', { value: 5000, configurable: true });
    Object.defineProperty(el, 'clientHeight', { value: 600, configurable: true });
    el.scrollTop = 4200;
    await w.find('.list').trigger('scroll');
    await flushPromises();
    expect(String(get.mock.calls[0][0])).toContain('page=2');
  });

  // 反过来也要成立：没有下一页还去请求，就是每滚一下打一个空页。
  it('到底了就不再请求', async () => {
    const { w } = await mk(page({ total: 2 }));
    get.mockClear();
    const el = w.find('.list').element as HTMLElement;
    Object.defineProperty(el, 'scrollHeight', { value: 500, configurable: true });
    Object.defineProperty(el, 'clientHeight', { value: 600, configurable: true });
    el.scrollTop = 0;
    await w.find('.list').trigger('scroll');
    await flushPromises();
    expect(get).not.toHaveBeenCalled();
  });
});

describe('FilesView 操作', () => {
  it('选中后选择栏给出数量与动作', async () => {
    const { w } = await mk();
    await w.findAllComponents(FileRow)[0].find('.chk').trigger('click');
    expect(w.find('.selbar').text()).toContain('1 项');
  });

  it('全选 / 取消全选', async () => {
    const { w } = await mk();
    await w.findAllComponents(FileRow)[0].find('.chk').trigger('click');
    const btns = () => w.findAll('.selbar .tb');
    await btns()[0].trigger('click');
    expect(useFilesStore().selected).toEqual(['a.txt', 'sub']);
    await btns()[0].trigger('click');
    expect(useFilesStore().selected).toEqual([]);
  });

  // 删除必须走队列（POST /fs/delete 回 job_id），而且默认 permanent=false。
  it('删除先确认，再提交任务且默认进回收站', async () => {
    const { w } = await mk();
    await w.findAllComponents(FileRow)[0].find('.chk').trigger('click');
    await w.findAll('.selbar .tb')[3].trigger('click');
    expect(w.text()).toContain('移入回收站');
    const ok = w.findAll('.sheet .tb, .shf .tb, .tb.go');
    await ok[ok.length - 1].trigger('click');
    await flushPromises();
    const call = post.mock.calls.find((c) => c[0] === '/api/fs/delete');
    expect(call?.[1]).toEqual({ paths: ['/data/a.txt'], permanent: false });
  });

  it('勾了永久删除就带 permanent=true', async () => {
    const { w } = await mk();
    await w.findAllComponents(FileRow)[0].find('.chk').trigger('click');
    await w.findAll('.selbar .tb')[3].trigger('click');
    await w.find('.pm input').setValue(true);
    const ok = w.findAll('.tb.go');
    await ok[ok.length - 1].trigger('click');
    await flushPromises();
    expect(post.mock.calls.find((c) => c[0] === '/api/fs/delete')?.[1]).toMatchObject({ permanent: true });
  });

  // 剪切/复制本身**不发请求**（只是记下路径），点粘贴才提交任务。
  // 这条把职责边界钉住：粘贴的目标永远是"当前正在看的目录",
  // 而不是剪贴板里那些路径各自的原目录（那就成了原地复制）。
  it('剪切只写剪贴板，点粘贴才提交 move 任务且目标是当前目录', async () => {
    const { w } = await mk();
    await w.findAllComponents(FileRow)[0].find('.chk').trigger('click');
    await w.findAll('.selbar .tb')[1].trigger('click');
    await flushPromises();
    expect(post).not.toHaveBeenCalled();
    expect(useFilesStore().clip).toEqual({ mode: 'cut', paths: ['/data/a.txt'] });
    await w.findAll('.tools .tb')[1].trigger('click'); // 工具栏粘贴
    await flushPromises();
    const call = post.mock.calls.find((c) => c[0] === '/api/fs/jobs');
    expect(call?.[1]).toMatchObject({ op: 'move', paths: ['/data/a.txt'], dst: '/data' });
  });

  it('复制粘贴提交的是 copy，且剪贴板留着（可反复粘到别的目录）', async () => {
    const { w } = await mk();
    await w.findAllComponents(FileRow)[0].find('.chk').trigger('click');
    await w.findAll('.selbar .tb')[2].trigger('click');
    await w.findAll('.tools .tb')[1].trigger('click');
    await flushPromises();
    expect(post.mock.calls.find((c) => c[0] === '/api/fs/jobs')?.[1]).toMatchObject({ op: 'copy' });
    expect(useFilesStore().canPaste).toBe(true);
  });

  it('剪贴板有内容时工具栏粘贴按钮才可用', async () => {
    const { w } = await mk();
    expect(w.findAll('.tools .tb')[1].attributes('disabled')).toBeDefined();
    await w.findAllComponents(FileRow)[0].find('.chk').trigger('click');
    await w.findAll('.selbar .tb')[2].trigger('click'); // 复制
    expect(w.findAll('.tools .tb')[1].attributes('disabled')).toBeUndefined();
  });

  it('右键空白处给出"新建目录"，创建时打 /fs/mkdir', async () => {
    const { w } = await mk();
    const ev: any = new Event('contextmenu', { bubbles: true, cancelable: true });
    ev.clientX = 10;
    ev.clientY = 10;
    w.find('.list').element.dispatchEvent(ev);
    await flushPromises();
    expect(w.text()).toContain('新建目录');
    await w.findAll('.menu .mi')[0].trigger('click');
    await w.find('.fi').setValue('新建文件夹');
    const ok = w.findAll('.tb.go');
    await ok[ok.length - 1].trigger('click');
    await flushPromises();
    expect(post.mock.calls.some((c) => c[0] === '/api/fs/mkdir' && c[1].path === '/data/新建文件夹')).toBe(true);
  });

  it('右键文件选"重命名"改名字', async () => {
    const { w } = await mk();
    const row = w.findAllComponents(FileRow)[0];
    const ev: any = new Event('contextmenu', { bubbles: true, cancelable: true });
    ev.clientX = 20;
    ev.clientY = 20;
    row.find('.row').element.dispatchEvent(ev);
    await flushPromises();
    const items = w.findAll('.menu .mi');
    const rename = items.find((i) => i.text().includes('重命名'));
    await rename!.trigger('click');
    await w.find('.fi').setValue('b.txt');
    const ok = w.findAll('.tb.go');
    await ok[ok.length - 1].trigger('click');
    await flushPromises();
    expect(post.mock.calls.some((c) => c[0] === '/api/fs/rename' && c[1].from === '/data/a.txt' && c[1].to === 'b.txt')).toBe(true);
  });

  // 剪贴板里是 cut 时，源条目要压暗：否则用户看不出哪些"等着被移走"。
  it('剪切后源条目压暗', async () => {
    const { w } = await mk();
    await w.findAllComponents(FileRow)[0].find('.chk').trigger('click');
    await w.findAll('.selbar .tb')[1].trigger('click');
    await flushPromises();
    expect(w.findAllComponents(FileRow)[0].find('.row').classes()).toContain('cut');
  });

  it('提交任务后给出可见提示（不能点了没反应）', async () => {
    const { w } = await mk();
    await w.findAllComponents(FileRow)[0].find('.chk').trigger('click');
    await w.findAll('.selbar .tb')[3].trigger('click');
    const ok = w.findAll('.tb.go');
    await ok[ok.length - 1].trigger('click');
    await flushPromises();
    expect(w.find('.hint').text()).toContain('任务已提交');
  });

  it('回收站按钮打开面板', async () => {
    const { w } = await mk();
    await w.findAll('.tools .tb')[3].trigger('click');
    await flushPromises();
    expect(w.text()).toContain('回收站是空的');
  });

  it('在终端中打开：带着目录跳到终端页', async () => {
    const { w, router } = await mk();
    const row = w.findAllComponents(FileRow)[1]; // sub 是目录
    const ev: any = new Event('contextmenu', { bubbles: true, cancelable: true });
    ev.clientX = 20;
    ev.clientY = 20;
    row.find('.row').element.dispatchEvent(ev);
    await flushPromises();
    const item = w.findAll('.menu .mi').find((i) => i.text().includes('在终端中打开'));
    await item!.trigger('click');
    await flushPromises();
    expect(router.currentRoute.value.query.cwd).toBe('/data/sub');
  });
});

describe('FilesView 上传', () => {
  it('上传按钮打开文件选择框', async () => {
    const { w } = await mk();
    // 文件输入框是 hidden 的，只能靠程序 click；这里替掉它是因为
    // happy-dom 的 click 会真的走"打开文件选择器"那条路径。
    const el = w.find('input[type=file]').element as HTMLInputElement;
    const spy = vi.fn();
    el.click = spy;
    await w.findAll('.tools .tb')[2].trigger('click');
    expect(spy).toHaveBeenCalled();
  });

  // 冲突必须给出三条出路（覆盖/换名/跳过）—— 后端只有这三种策略,
  // 少一条界面上就有一种"只能取消"的死局。
  it('同名冲突时给出覆盖 / 换名字 / 跳过', async () => {
    const { w } = await mk();
    const uploads = (await import('../stores/uploads')).useUploadsStore();
    uploads.items.push({
      id: 'u1', dir: '/data', name: 'a.txt', size: 10, received: 0, status: 'conflict', slice: () => new Blob(),
    });
    await flushPromises();
    const t = w.text();
    expect(t).toContain('覆盖');
    expect(t).toContain('跳过');
    expect(t).toContain('换个名字');
  });
});
