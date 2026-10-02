import { describe, it, expect, beforeEach, vi } from 'vitest';
import { setActivePinia, createPinia } from 'pinia';
import { setApi, resetApi } from '../api/inject';
import { useFilesStore } from '../stores/files';
import { useFsJobsStore } from '../stores/fsJobs';
import type { FsEntry, FsRoot, ListPage } from '../api/files';

const location = { pathname: '/', search: '', assign: vi.fn() };

function entry(name: string, over: Partial<FsEntry> = {}): FsEntry {
  return {
    name, is_dir: false, is_symlink: false, size: 10, mtime: 0,
    mime: 'text/plain', mode: '-rw-r--r--', ...over,
  };
}

function page(over: Partial<ListPage> = {}): ListPage {
  return { path: '/data', page: 1, size: 200, total: 2, entries: [entry('a'), entry('b')], ...over };
}

// 记录每次请求的完整 URL，好断言查询参数（后端读的是 page/size/sort/order/show_hidden）。
let urls: string[] = [];
let bodies: any[] = [];

function fakeApi(handler?: (url: string, body?: any) => any) {
  urls = [];
  bodies = [];
  return {
    get: vi.fn(async (url: string) => {
      urls.push('GET ' + url);
      if (url.startsWith('/api/fs/roots')) return { roots: [] as FsRoot[] };
      return handler ? handler(url) : page();
    }),
    post: vi.fn(async (url: string, body?: any) => {
      urls.push('POST ' + url);
      bodies.push(body);
      return handler ? handler(url, body) : { job_id: 7 };
    }),
    del: vi.fn(async (url: string) => {
      urls.push('DELETE ' + url);
      return null;
    }),
    patch: vi.fn(async () => ({})),
    put: vi.fn(async () => ({})),
  };
}

beforeEach(() => {
  setActivePinia(createPinia());
  resetApi();
});

describe('files store：目录加载', () => {
  it('open 用响应回显的 path，而不是发出去的参数', async () => {
    // 符号链接会让两者指向不同目录，而 selectedPaths 用的是 dir：
    // 不一致时一次删除会打到隔壁目录的同名文件上。
    setApi(fakeApi(() => page({ path: '/real/target' })) as never, location);
    const s = useFilesStore();
    await s.open('/link/to/dir');
    expect(s.dir).toBe('/real/target');
    // open 会清选中（跨目录误删的防线），所以要选一次再看绝对路径
    // 是不是用回显目录拼的 —— 而不是发出去的那个 /link/to/dir。
    s.toggleSelect('a');
    expect(s.selectedPaths).toEqual(['/real/target/a']);
  });

  it('加载失败时保留旧列表，只置 error', async () => {
    let boom = false;
    setApi(
      fakeApi((url) => {
        if (boom) throw { code: 'server', message: '磁盘错误', status: 500 };
        return page();
      }) as never,
      location,
    );
    const s = useFilesStore();
    await s.open('/data');
    expect(s.entries).toHaveLength(2);
    boom = true;
    await s.open('/elsewhere');
    // 清空列表等于告诉用户"文件凭空没了"，他会去刷新、然后按删除。
    expect(s.entries).toHaveLength(2);
    expect(s.error).toBe('磁盘错误');
  });

  it('换目录清空选中（跨目录误删的唯一防线）', async () => {
    setApi(fakeApi(() => page()) as never, location);
    const s = useFilesStore();
    await s.open('/data');
    s.toggleSelect('a');
    expect(s.selected).toEqual(['a']);
    await s.open('/other');
    expect(s.selected).toEqual([]);
  });

  it('查询参数带 page/size/sort/order/show_hidden', async () => {
    setApi(fakeApi() as never, location);
    const s = useFilesStore();
    s.hidden = true;
    await s.open('/data');
    const u = urls.find((x) => x.startsWith('GET /api/fs/list'));
    expect(u).toContain('path=%2Fdata');
    expect(u).toContain('page=1');
    expect(u).toContain('size=200');
    expect(u).toContain('show_hidden=1');
  });

  it('setSort 同列再点翻方向，并重新拉取（排序在后端做）', async () => {
    setApi(fakeApi() as never, location);
    const s = useFilesStore();
    await s.open('/data');
    const before = urls.length;
    await s.setSort('size');
    expect(s.sort).toBe('size');
    expect(s.desc).toBe(false);
    await s.setSort('size');
    expect(s.desc).toBe(true);
    // 两次都发了请求：本地排一页会把"没加载的后几页"留在原位。
    expect(urls.length).toBe(before + 2);
    expect(urls[urls.length - 1]).toContain('order=desc');
  });

  it('loadMore 追加而不替换；到顶后不再发请求', async () => {
    setApi(
      fakeApi((url) =>
        url.includes('page=2')
          ? page({ page: 2, total: 3, entries: [entry('c')] })
          : page({ total: 3 }),
      ) as never,
      location,
    );
    const s = useFilesStore();
    await s.open('/data');
    expect(s.hasMore).toBe(true);
    await s.loadMore();
    expect(s.entries.map((e) => e.name)).toEqual(['a', 'b', 'c']);
    expect(s.hasMore).toBe(false);
    const n = urls.length;
    await s.loadMore();
    expect(urls.length).toBe(n); // 没有下一页就别再打一枪
  });
});

describe('files store：选择与剪贴板', () => {
  it('selectedPaths 用当前目录拼绝对路径', async () => {
    setApi(fakeApi() as never, location);
    const s = useFilesStore();
    await s.open('/data');
    s.toggleSelect('a');
    s.toggleSelect('b');
    expect(s.selectedPaths).toEqual(['/data/a', '/data/b']);
  });

  it('全选再点一次取消全选', async () => {
    setApi(fakeApi() as never, location);
    const s = useFilesStore();
    await s.open('/data');
    s.selectAll();
    expect(s.allSelected).toBe(true);
    s.selectAll();
    expect(s.selected).toEqual([]);
  });

  // 剪切 = 移动。提交完必须清剪贴板：源已经不在了，留着下一次粘贴只会
  // 报"源不存在"，而用户看不出为什么。
  it('粘贴剪切：提交 move 到当前目录，成功后清剪贴板', async () => {
    setApi(fakeApi() as never, location);
    const s = useFilesStore();
    await s.open('/data');
    s.toggleSelect('a');
    s.setClip('cut');
    const jobs = useFsJobsStore();
    jobs.items = [];
    const id = await s.paste();
    expect(id).toBe(7);
    expect(bodies[0]).toEqual({ op: 'move', paths: ['/data/a'], dst: '/data' });
    expect(s.clip).toBeNull();
  });

  // 复制可以反复粘到不同目录 —— 那正是复制的语义，不能跟着清。
  it('粘贴复制：提交 copy 且保留剪贴板', async () => {
    setApi(fakeApi() as never, location);
    const s = useFilesStore();
    await s.open('/data');
    s.toggleSelect('a');
    s.setClip('copy');
    await s.paste();
    expect(bodies[0].op).toBe('copy');
    expect(s.clip).not.toBeNull();
    expect(s.canPaste).toBe(true);
  });

  it('没选中时 setClip 不写剪贴板（粘贴按钮该禁用）', async () => {
    setApi(fakeApi() as never, location);
    const s = useFilesStore();
    await s.open('/data');
    s.setClip('cut');
    expect(s.clip).toBeNull();
    expect(s.canPaste).toBe(false);
  });
});

describe('files store：删除 / 建目录 / 改名', () => {
  it('删除提交选中项的绝对路径，回 job_id', async () => {
    setApi(fakeApi() as never, location);
    const s = useFilesStore();
    await s.open('/data');
    s.toggleSelect('a');
    const id = await s.deleteSelected();
    expect(id).toBe(7);
    expect(bodies[0]).toEqual({ paths: ['/data/a'], permanent: false });
    expect(s.selected).toEqual([]);
  });

  // 回收站建不起来（只读盘等）：后端给 422 + 固定 code，界面上唯一
  // 可行的出路是永久删除。靠中文文案子串匹配会在改文案时静默失效，
  // 所以这里断言的是 code 旗标。
  it('trash_unwritable 置旗标（不是只留一行文案）', async () => {
    setApi(
      fakeApi((url) => {
        // 只有 delete 这一发抛错。让 GET /fs/list 也抛的话，open 会先
        // 置上 trashUnwritable，于是"deleteSelected 忘了置旗标"这个 bug
        // 就测不到了（旗标被 open 顺手置上了）。
        if (url.includes('/api/fs/delete')) {
          throw { code: 'trash_unwritable', message: '该盘只能永久删除', status: 422 };
        }
        return page();
      }) as never,
      location,
    );
    const s = useFilesStore();
    await s.open('/data');
    s.toggleSelect('a');
    await expect(s.deleteSelected()).rejects.toBeTruthy();
    expect(s.trashUnwritable).toBe(true);
    expect(s.error).toContain('永久删除');
  });

  it('建目录拼在当前目录下', async () => {
    setApi(fakeApi() as never, location);
    const s = useFilesStore();
    await s.open('/data');
    await s.mkdir('newdir');
    expect(bodies[0]).toEqual({ path: '/data/newdir' });
  });

  it('改名发 from（绝对）+ to（新名）', async () => {
    setApi(fakeApi() as never, location);
    const s = useFilesStore();
    await s.open('/data');
    await s.rename('a', 'b');
    expect(bodies[0]).toEqual({ from: '/data/a', to: 'b' });
  });
});

describe('files store：回收站', () => {
  it('列表 null 兜成空数组', async () => {
    setApi(fakeApi(() => ({ items: null })) as never, location);
    const s = useFilesStore();
    expect(await s.loadTrash()).toEqual([]);
  });

  // 危险操作服务端强制 ?confirm=1（设计 486）：漏了就是每个调用点
  // 都收到 400，而 400 在界面上只是"操作失败"四个字。
  it('清空与永久删除都带 confirm=1', async () => {
    setApi(fakeApi(() => ({ removed: 3 })) as never, location);
    const s = useFilesStore();
    expect(await s.emptyTrash()).toBe(3);
    await s.purgeTrash('abc/1');
    expect(urls.some((u) => u.includes('/api/fs/trash/empty?confirm=1'))).toBe(true);
    expect(urls.some((u) => u === 'DELETE /api/fs/trash/abc%2F1?confirm=1')).toBe(true);
  });
});
