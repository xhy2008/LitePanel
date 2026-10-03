import { beforeEach, describe, expect, it, vi } from 'vitest';
import { createPinia, setActivePinia } from 'pinia';
import { flushPromises, mount } from '@vue/test-utils';
import { setApi, resetApi } from '../api/inject';
import { useFsJobsStore } from '../stores/fsJobs';
import { attachJobStream } from '../composables/useJobStream';
import JobDrawer from '../components/jobs/JobDrawer.vue';
import AppIcon from '../components/AppIcon.vue';
import { hasIcon } from '../components/iconPaths';
import type { JobProgress, JobRow } from '../api/fsJobs';

const location = { pathname: '/', search: '', assign: () => {} };

function row(over: Partial<JobRow> = {}): JobRow {
  return {
    id: 1, op: 'delete', src: ['/data/a.txt'], dst: '',
    total_bytes: 0, done_bytes: 0, entries_total: 10, entries_done: 0,
    state: 'running', cancel_requested: false, permanent: false,
    resumed: false, created_at: 0, updated_at: 0, ...over,
  };
}

function fakeApi(items: JobRow[] = []) {
  return {
    get: vi.fn(async () => ({ jobs: items })),
    post: vi.fn(async () => ({ ...row(), job_id: 1 })),
    patch: vi.fn(async () => ({})),
    put: vi.fn(async () => ({})),
    del: vi.fn(async () => ({ canceled: true })),
  };
}

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
  };
}

beforeEach(() => {
  setActivePinia(createPinia());
  resetApi();
});

describe('attachJobStream', () => {
  it('订阅 fsjobs 频道（频道名写错就一条也收不到）', () => {
    setApi(fakeApi() as never, location);
    const store = useFsJobsStore();
    const w = fakeWs();
    const stop = attachJobStream({ store, wsClient: w });
    expect(w.channels()).toContain('fsjobs');
    stop();
    expect(w.channels()).not.toContain('undefined');
  });

  // 这条是整个前端的立足点：进度推不落进 store，抽屉就永远停在旧数字，
  // 而任务在后台跑得好好的 —— 后端全绿、编译通过，只有界面看得见。
  it('fsjobs 帧推进 store', () => {
    setApi(fakeApi() as never, location);
    const store = useFsJobsStore();
    store.items = [row()];
    const w = fakeWs();
    attachJobStream({ store, wsClient: w });
    w.emit('fsjobs', {
      id: 1, op: 'delete', dst: '', total_bytes: 0, done_bytes: 0,
      entries_total: 10, entries_done: 7, state: 'running',
      cancel_requested: false, permanent: false, resumed: false, updated_at: 0,
    } satisfies JobProgress);
    expect(store.items[0].entries_done).toBe(7);
  });

  it('退订后不再改 store', () => {
    setApi(fakeApi() as never, location);
    const store = useFsJobsStore();
    store.items = [row()];
    const w = fakeWs();
    const stop = attachJobStream({ store, wsClient: w });
    stop();
    w.emit('fsjobs', { id: 1, entries_done: 9, state: 'done' });
    expect(store.items[0].entries_done).toBe(0);
  });
});

// 抽屉默认是收起的（设计 404："任何页面右下角可唤起"），所以看内容前
// 得先点浮动按钮。列表本身在挂载时就拉好了 —— 角标数字不打开也得准。
async function openDrawer(items: JobRow[] = []) {
  const api = fakeApi(items);
  setApi(api as never, location);
  const w = mount(JobDrawer, { props: { wsClient: fakeWs() as never } });
  await flushPromises();
  await w.find('.job-fab').trigger('click');
  await flushPromises();
  return { w, api };
}

describe('JobDrawer', () => {
  // 挂载就拉一次，与开没开无关：角标要显示"几个进行中"，那需要数据。
  it('挂载即拉列表；打开后渲染任务', async () => {
    const { w, api } = await openDrawer([row({ entries_done: 3 }), row({ id: 2, op: 'copy', state: 'running' })]);
    expect(api.get).toHaveBeenCalledWith('/api/fs/jobs');
    expect(w.text()).toContain('删除');
    expect(w.text()).toContain('复制');
  });

  // 进度条：分母是条目数（total_bytes 后端恒 0）。
  it('进行中任务显示百分比', async () => {
    const { w } = await openDrawer([row({ entries_total: 4, entries_done: 1 })]);
    expect(w.text()).toContain('25');
  });

  it('cancel_requested 显示"正在取消"而不是"进行中"', async () => {
    const { w } = await openDrawer([row({ state: 'running', cancel_requested: true })]);
    // 必须是"正在取消"这句话本身，而不是随便一个含"取消"的字样。
    expect(w.text()).toContain('正在取消');
  });

  // 已完成的项不再显示（用户的裁定：任务列表不记录已完成的任务）。
  //
  // 注意这是**显示层**的过滤，不是从 store 里删掉：
  //  · 文件页靠"watch items 里 active → 终态"这个边沿来刷新列表,
  //    真删掉会让那一帧根本进不到 store,刷新静默失效。
  //  · 后端那份记录才是"关浏览器也不中断"的凭据,前端无权抹。
  it('已完成的任务不出现在抽屉里', async () => {
    const { w } = await openDrawer([
      row({ id: 1, state: 'done' }),
      row({ id: 2, op: 'copy', state: 'running' }),
    ]);
    expect(w.text()).toContain('复制'); // 进行中的在
    expect(w.text()).not.toContain('已完成'); // done 那条不显示
    expect(w.findAll('.job-row')).toHaveLength(1);
  });

  // 全部完成 = 抽屉回到空状态文案,而不是留一排"已完成"。
  it('任务全部完成后抽屉显示空状态', async () => {
    const { w } = await openDrawer([row({ state: 'done' })]);
    expect(w.findAll('.job-row')).toHaveLength(0);
    expect(w.text()).toContain('还没有任务');
  });

  // 失败/中断必须留着：那两类带着错误原文和重试按钮,是用户唯一能
  // 据此决定"再跑一遍还是先腾磁盘"的地方。
  it('失败与中断的任务仍然显示', async () => {
    const { w } = await openDrawer([
      row({ id: 1, state: 'failed', error: 'no space left on device' }),
      row({ id: 2, op: 'copy', state: 'interrupted' }),
    ]);
    expect(w.findAll('.job-row')).toHaveLength(2);
    expect(w.text()).toContain('no space left on device');
  });

  // 推送把任务推到 done 之后,它应当立刻从抽屉消失（不用手动清）。
  it('任务跑到 done 后自动从抽屉消失', async () => {
    const { w } = await openDrawer([row({ entries_done: 3, entries_total: 10 })]);
    expect(w.findAll('.job-row')).toHaveLength(1);
    const store = useFsJobsStore();
    store.applyProgress({
      id: 1, op: 'delete', dst: '', total_bytes: 0, done_bytes: 0,
      entries_total: 10, entries_done: 10, state: 'done', cancel_requested: false,
      permanent: false, resumed: false, updated_at: 1,
    } as never);
    await flushPromises();
    expect(w.findAll('.job-row')).toHaveLength(0);
    // 但 store 里那一行还在 —— 文件页的列表刷新要读它。
    expect(store.items.some((j) => j.state === 'done')).toBe(true);
  });

  // 只有 pending/running 有取消按钮：给一条已完成的任务挂"取消"，
  // 点下去后端回 409，用户只会以为面板坏了。
  it('终态任务没有取消按钮', async () => {
    const { w } = await openDrawer([row({ state: 'done' })]);
    expect(w.find('.job-cancel').exists()).toBe(false);
  });

  // 重试按钮只在 interrupted 上出现（设计 403 行）。
  it('interrupted 任务给出重试按钮，点了提交到 retry', async () => {
    const { w, api } = await openDrawer([row({ state: 'interrupted', error: '面板关闭，任务未完成' })]);
    const btn = w.find('.job-retry');
    expect(btn.exists()).toBe(true);
    await btn.trigger('click');
    await flushPromises();
    expect(api.post).toHaveBeenCalledWith('/api/fs/jobs/1/retry');
  });

  it('点取消提交 DELETE 且按钮进入禁用态（防连点）', async () => {
    const { w, api } = await openDrawer([row({ state: 'running' })]);
    await w.find('.job-cancel').trigger('click');
    await flushPromises();
    expect(api.del).toHaveBeenCalledWith('/api/fs/jobs/1');
  });

  it('失败任务把后端错误原文显示出来（不吞进"失败"两个字）', async () => {
    const { w } = await openDrawer([row({ state: 'failed', error: 'no space left on device' })]);
    expect(w.text()).toContain('no space left on device');
  });

  it('没有任务时显示空态而不是空白', async () => {
    const { w } = await openDrawer([]);
    expect(w.find('.job-empty').exists()).toBe(true);
  });
});

// 抽屉用到的图标名字。AppIcon 对未知名字是 PATHS[name] ?? []，静默画
// 空 SVG —— "图标没了"在页面上几乎看不出，只能靠测试钉住用到的名字。
describe('JobDrawer 用到的图标都有图形', () => {
  const used = ['receipt', 'copy', 'move', 'trash'];
  it.each(used)('%s', (name) => {
    expect(hasIcon(name), `图标 ${name} 缺路径定义`).toBe(true);
    const w = mount(AppIcon, { props: { name } });
    expect(w.find('path').exists()).toBe(true);
  });
});
