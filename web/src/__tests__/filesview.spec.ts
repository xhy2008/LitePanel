import { beforeEach, describe, expect, it, vi } from 'vitest';
import { flushPromises, mount } from '@vue/test-utils';
import { createPinia, setActivePinia } from 'pinia';
import { createRouter, createMemoryHistory } from 'vue-router';
import FilesView from '../views/FilesView.vue';
import FileRow from '../components/files/FileRow.vue';
import { setApi, resetApi } from '../api/inject';
import { useFilesStore } from '../stores/files';
import { useFsJobsStore } from '../stores/fsJobs';
import type { JobOp, JobState, JobRow, JobProgress } from '../api/fsJobs';
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

// 选择栏里的按钮按**文案**找，不按位置索引。
//
// 之前用的是 findAll('.selbar .tb')[3] 这种索引：往选择栏加一个按钮
// （比如"下载"）就会让 11 处索引全体错位 —— 测试会点到隔壁按钮上却
// 照样通过，那比失败更糟。
function barBtn(w: Awaited<ReturnType<typeof mk>>['w'], label: string) {
  const btn = w.findAll('.selbar .tb').find((b) => b.text().includes(label));
  if (!btn) throw new Error(`选择栏里没有「${label}」按钮`);
  return btn;
}

// 真实 WS 推送跨帧到达；settle() 走一个宏任务，确保上一帧先被
// 组件的 watch 记录（Vue 会把同一 tick 内的多次改动合成一次回调）。
async function settle() {
  await new Promise((r) => setTimeout(r, 0));
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
    await barBtn(w, '全选').trigger('click');
    expect(useFilesStore().selected).toEqual(['a.txt', 'sub']);
    await barBtn(w, '全选').trigger('click');
    expect(useFilesStore().selected).toEqual([]);
  });

  // 删除必须走队列（POST /fs/delete 回 job_id），而且默认 permanent=false。
  it('删除先确认，再提交任务且默认进回收站', async () => {
    const { w } = await mk();
    await w.findAllComponents(FileRow)[0].find('.chk').trigger('click');
    await barBtn(w, '删除').trigger('click');
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
    await barBtn(w, '删除').trigger('click');
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
    await barBtn(w, '剪切').trigger('click');
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
    await barBtn(w, '复制').trigger('click');
    await w.findAll('.tools .tb')[1].trigger('click');
    await flushPromises();
    expect(post.mock.calls.find((c) => c[0] === '/api/fs/jobs')?.[1]).toMatchObject({ op: 'copy' });
    expect(useFilesStore().canPaste).toBe(true);
  });

  it('剪贴板有内容时工具栏粘贴按钮才可用', async () => {
    const { w } = await mk();
    expect(w.findAll('.tools .tb')[1].attributes('disabled')).toBeDefined();
    await w.findAllComponents(FileRow)[0].find('.chk').trigger('click');
    await barBtn(w, '复制').trigger('click'); // 复制
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
    await barBtn(w, '剪切').trigger('click');
    await flushPromises();
    expect(w.findAllComponents(FileRow)[0].find('.row').classes()).toContain('cut');
  });

  it('提交任务后给出可见提示（不能点了没反应）', async () => {
    const { w } = await mk();
    await w.findAllComponents(FileRow)[0].find('.chk').trigger('click');
    await barBtn(w, '删除').trigger('click');
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

describe('FilesView 任务完成后的列表刷新', () => {
  // 删除/粘贴走的是后台队列：界面提交完任务之后如果就此"失聪",
  // 用户删完看到的还是原来那一列表（文件明明已从盘上没了），第一反应是
  // 没删掉、会再删一次。队列的正确闭环 = 任务到终态时把列表刷回来。
  async function jobs0() {
    return (await import('../stores/fsJobs')).useFsJobsStore();
  }
  // 推进一条 running 任务再让下一帧变终态。running 与终态之间必须 settle():
  // watch 靠"上一帧是活跃"记边沿，两帧塞进同一个 tick 会被 Vue 合成一次
  // 回调、只看到终态，边沿判断不成立。真实 WS 推送本来就是分帧到达的",-
  // -settle 是在如实建模，不是给实现打补丁。
  async function finish(fsJobs: ReturnType<typeof useFsJobsStore>, running: JobRow, end: JobProgress) {
    fsJobs.items.push(running);
    await settle();
    fsJobs.applyProgress(end);
    await flushPromises();
  }

  function runningJob(id: number, op: JobOp, src: string[], dst = ''): JobRow {
    return {
      id, op, src, dst, total_bytes: 0, done_bytes: 0, entries_total: 1, entries_done: 0,
      state: 'running', cancel_requested: false, permanent: false, resumed: false,
      created_at: 0, updated_at: 0,
    };
  }
  function endJob(id: number, op: JobOp, state: JobState, dst = '', error = ''): JobProgress {
    return {
      id, op, dst, total_bytes: 0, done_bytes: 0, entries_total: 1,
      entries_done: state === 'done' ? 1 : 0, state, cancel_requested: false,
      permanent: false, resumed: false, error, updated_at: 1,
    };
  }

  it('删除任务完成后列表自动刷新', async () => {
    const { w } = await mk();
    await w.findAllComponents(FileRow)[0].find('.chk').trigger('click');
    await barBtn(w, '删除').trigger('click');
    const ok = w.findAll('.tb.go');
    await ok[ok.length - 1].trigger('click');
    await flushPromises();
    const fsJobs = await jobs0();
    get.mockClear();
    await finish(fsJobs, runningJob(7, 'delete', ['/data/a.txt']), endJob(7, 'delete', 'done'));
    expect(get.mock.calls.some((c) => String(c[0]).startsWith('/api/fs/list'))).toBe(true);
  });

  // 与 stores/uploads 的 refresh 同一条规则：只在用户**正看着**那个目录时
  // 刷新。无脑刷新会把他正在翻的目录弹回第一页。
  it('用户已经离开那个目录时不刷新', async () => {
    const { w } = await mk();
    await w.findAllComponents(FileRow)[0].find('.chk').trigger('click');
    await barBtn(w, '删除').trigger('click');
    const ok = w.findAll('.tb.go');
    await ok[ok.length - 1].trigger('click');
    await flushPromises();
    const fsJobs = await jobs0();
    fsJobs.items.push(runningJob(8, 'delete', ['/data/a.txt']));
    await settle();
    // 显式跳到别的目录：只关心"当前正看着哪个目录",与面包屑怎么渲染无关。
    get.mockResolvedValue({ path: '/elsewhere', page: 1, size: 500, total: 0, entries: [] } as never);
    await useFilesStore().open('/elsewhere');
    await flushPromises();
    get.mockClear();
    fsJobs.applyProgress(endJob(8, 'delete', 'done') as never);
    await flushPromises();
    expect(get).not.toHaveBeenCalled();
  });

  // 粘贴（copy）的目标目录同样：完成之后新东西要出现在列表里。
  it('粘贴任务完成后刷新目标目录', async () => {
    const { w } = await mk();
    await w.findAllComponents(FileRow)[0].find('.chk').trigger('click');
    await barBtn(w, '复制').trigger('click');
    await w.findAll('.tools .tb')[1].trigger('click');
    await flushPromises();
    const fsJobs = await jobs0();
    get.mockClear();
    await finish(fsJobs, runningJob(9, 'copy', ['/data/a.txt'], '/data'), endJob(9, 'copy', 'done', '/data'));
    expect(get.mock.calls.some((c) => String(c[0]).startsWith('/api/fs/list'))).toBe(true);
  });

  // 失败也刷新：copy 在后端不回滚（见 exec_run.go），失败可能已经把一部分
  // 文件放进目标目录。delete/move 失败时磁盘没变,多刷一次无害。宁多勿漏。
  it('任务失败时也会刷新（copy 不回滚）', async () => {
    const { w } = await mk();
    await w.findAllComponents(FileRow)[0].find('.chk').trigger('click');
    await barBtn(w, '删除').trigger('click');
    const ok = w.findAll('.tb.go');
    await ok[ok.length - 1].trigger('click');
    await flushPromises();
    const fsJobs = await jobs0();
    get.mockClear();
    await finish(fsJobs, runningJob(10, 'delete', ['/data/a.txt']),
      endJob(10, 'delete', 'failed', '', 'no space left on device'));
    expect(get.mock.calls.some((c) => String(c[0]).startsWith('/api/fs/list'))).toBe(true);
  });

  // 与本目录无关的任务完成时不刷新（比如另一个标签页提交的任务）。
  it('与本目录无关的任务完成时不刷新', async () => {
    const { w } = await mk();
    const fsJobs = await jobs0();
    get.mockClear();
    await finish(fsJobs, runningJob(11, 'delete', ['/elsewhere/x']), endJob(11, 'delete', 'done'));
    expect(get).not.toHaveBeenCalled();
  });
});

describe('FilesView 选择栏的下载按钮', () => {
  // 用户实测：选中之后底部那排按钮里没有下载，必须长按弹菜单才能下载。
  // 手机上「长按」是隐藏操作，而下载是最常用的动作之一，该在一键可达处。
  function spyClicks(): string[] {
    const got: string[] = [];
    vi.spyOn(HTMLAnchorElement.prototype, 'click').mockImplementation(function (
      this: HTMLAnchorElement,
    ) {
      got.push(this.href);
    });
    return got;
  }

  async function select(ks: number[]) {
    const r = await mk();
    const rows = r.w.findAllComponents(FileRow);
    for (const k of ks) await rows[k].find('.chk').trigger('click');
    return r;
  }

  it('选择栏里有下载按钮', async () => {
    const { w } = await select([0]);
    expect(w.findAll('.selbar .tb').some((b) => b.text().includes('下载'))).toBe(true);
  });

  // 单个文件走 /fs/download 直链，且带的是**绝对路径**。拼错参数名的表现
  // 是「点了没反应」而没有任何报错 —— 只能在这里钉住。
  it('选中单个文件时下载指向 /fs/download 与该项绝对路径', async () => {
    const clicks = spyClicks();
    const { w } = await select([0]);
    await barBtn(w, '下载').trigger('click');
    await flushPromises();
    expect(clicks).toHaveLength(1);
    expect(decodeURIComponent(clicks[0])).toContain('/api/fs/download?path=/data/a.txt');
  });

  // 选中目录必须改走 zip：后端的 download 对目录必然失败（它是文件流）,
  // 而 zip 对单文件也合法，所以「含目录」一律打包。
  it('选中项含目录时改走 /fs/zip', async () => {
    const clicks = spyClicks();
    const { w } = await select([1]); // sub 是目录
    await barBtn(w, '下载').trigger('click');
    await flushPromises();
    expect(decodeURIComponent(clicks[0])).toContain('/api/fs/zip?path=/data/sub');
  });

  it('多选走 zip 且一个 path 都不能少', async () => {
    const clicks = spyClicks();
    const { w } = await select([0, 1]);
    await barBtn(w, '下载').trigger('click');
    await flushPromises();
    const q = new URL(clicks[0]).searchParams.getAll('path');
    expect(q).toEqual(['/data/a.txt', '/data/sub']);
  });

  // 下载走 <a download>，浏览器自己处理进度与保存位置；这里点了之后
  // 选择必须还在（用户常常要接着对同一批文件做别的事）。
  it('点下载不清空选择', async () => {
    spyClicks();
    const { w } = await select([0]);
    await barBtn(w, '下载').trigger('click');
    await flushPromises();
    expect(useFilesStore().selected).toEqual(['a.txt']);
  });

  it('没选中时选择栏整个不显示（也就没有能点的下载按钮）', async () => {
    const { w } = await mk();
    expect(w.find('.selbar').exists()).toBe(false);
  });
});

describe('FilesView 属性面板', () => {
  // 用户实测：属性点进去只有一行路径，没什么用。要么给真信息，要么
  // 别占一个菜单项骗人点进去。
  async function openProps(w: Awaited<ReturnType<typeof mk>>['w'], onRow: number | null) {
    const target = onRow === null ? w.find('.list') : w.findAllComponents(FileRow)[onRow].find('.row');
    const ev: Event & { clientX?: number; clientY?: number } = new Event('contextmenu', {
      bubbles: true,
      cancelable: true,
    });
    ev.clientX = 24;
    ev.clientY = 24;
    target.element.dispatchEvent(ev);
    await flushPromises();
    const item = w.findAll('.menu .mi').find((i) => i.text().includes('属性'));
    if (!item) throw new Error('菜单里没有属性');
    await item.trigger('click');
    await flushPromises();
  }

  // 断言只看属性面板自己的文本：整个 view 的 text() 里包含列表的「大小」
  // 列，用它断言"不显示大小"永远为假（或永远为真），测不到东西。
  const propsText = (w: Awaited<ReturnType<typeof mk>>['w']) => w.find('.props').text();

  // 面板只读列表里已有的字段，不发任何请求。
  it('单个文件的属性给出大小/时间/权限/类型/完整路径', async () => {
    const { w } = await mk(page({ entries: [entry('a.txt', { size: 4096 }), entry('sub')] }));
    await openProps(w, 0);
    const t = propsText(w);
    expect(t).toContain('4.0 KB'); // 人类可读，不是裸字节
    expect(t).toContain('-rw-r--r--');
    expect(t).toContain('/data/a.txt'); // 属性里抄路径是常态
    expect(t).toContain('文本');
    expect(t).toContain('2023-'); // 属性里的时间带年份（列表里为了省地方省掉了）
    expect(t).not.toContain('undefined'); // 缺字段要留空，不能印 undefined
  });

  // 后端的 mime 是按扩展名查表来的，认不出来就报 application/octet-stream,
  // 目录则报 inode/directory。把它原样列进属性会有两个问题：绝大多数文件
  // 都显示同一行 octet-stream（零信息），而用户会以为面板读过文件内容 ——
  // 它没有。「类型」那一行已经把能确证的说清楚了。
  it('不显示 mime 行（避免冒充内容嗅探的结果）', async () => {
    const { w } = await mk(
      page({ entries: [entry('a.txt', { size: 4096, mime: 'application/octet-stream' }), entry('sub')] }),
    );
    await openProps(w, 0);
    expect(propsText(w)).not.toContain('MIME');
    expect(propsText(w)).not.toContain('octet-stream');
  });

  // 目录的 size 是 inode 大小（几百字节），当成「大小」显示是骗人：用户会
  // 以为一个万文件的目录只有几百字节。所以这一行干脆不给。
  it('目录不给大小这一行', async () => {
    const { w } = await mk();
    await openProps(w, 1);
    expect(propsText(w)).toContain('目录');
    expect(propsText(w)).not.toContain('大小');
  });

  it('多选给出条目数与文件总大小（目录不计入）', async () => {
    const { w } = await mk(
      page({ entries: [entry('a.txt', { size: 4096 }), entry('sub', { size: 3 * 1024 * 1024 })] }),
    );
    await w.findAllComponents(FileRow)[0].find('.chk').trigger('click');
    await w.findAllComponents(FileRow)[1].find('.chk').trigger('click');
    await openProps(w, null);
    const t = propsText(w);
    expect(t).toContain('2 项');
    // 只有 a.txt 计入。sub 的 inode 大小故意给到 3 MB：要是把目录也加进
    // 来，总量就变成 3.0 MB —— 差一个量级，这个断言才咬得住。
    expect(t).toContain('4.0 KB');
    expect(t).not.toContain('3.0 MB');
    expect(t).toContain('目录大小未统计');
  });

  it('多选给出条目数与文件总大小', async () => {
    const { w } = await mk();
    await w.findAllComponents(FileRow)[0].find('.chk').trigger('click');
    await w.findAllComponents(FileRow)[1].find('.chk').trigger('click');
    await openProps(w, null);
    const t = w.text();
    expect(t).toContain('2');
    // 目录不计入总大小（理由同上），只有 a.txt 的 10 B。
    expect(t).toContain('10 B');
  });

  // 属性面板只读列表里已有的字段，不发任何请求：列表项本来就是 lstat 的
  // 结果，再问一次后端只会多一次往返 + 一个能失败的环节。
  it('属性面板不发任何请求', async () => {
    const { w } = await mk();
    get.mockClear();
    await openProps(w, 0);
    expect(get).not.toHaveBeenCalled();
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
