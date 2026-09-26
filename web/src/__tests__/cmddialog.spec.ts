import { describe, expect, it, vi, beforeEach } from 'vitest';
import { mount, flushPromises } from '@vue/test-utils';
import { createPinia, setActivePinia } from 'pinia';
import { setApi, resetApi } from '../api/inject';
import AddCommandDialog from '../components/quickcmd/AddCommandDialog.vue';
import type { CommandRow } from '../api/quickcmd';

const location = { pathname: '/quick', search: '', assign: () => {} };

function row(over: Partial<CommandRow> = {}): CommandRow {
  return {
    id: 1, name: '看盘', command: 'df -h', cwd: '', need_confirm: false, sort: 10, created_at: 0, ...over,
  };
}

function fakeApi() {
  const posts: { p: string; b: unknown }[] = [];
  const patches: { p: string; b: unknown }[] = [];
  return {
    posts,
    patches,
    api: {
      get: vi.fn(async () => ({ commands: [] })),
      post: vi.fn(async (p: string, b: unknown) => {
        posts.push({ p, b });
        return row();
      }),
      patch: vi.fn(async (p: string, b: unknown) => {
        patches.push({ p, b });
        return row();
      }),
      put: vi.fn(async () => ({})),
      del: vi.fn(async () => ({ ok: true })),
    },
  };
}

let f: ReturnType<typeof fakeApi>;

beforeEach(() => {
  setActivePinia(createPinia());
  resetApi();
  f = fakeApi();
  setApi(f.api as never, location);
});

// 必填校验在发请求之前：让后端拒绝虽然结果一样，但用户要等一个往返才
// 知道"名字没填"，原因还被埋进一句 error 文案里。
it('名称或命令为空时不发请求，直接说缺什么', async () => {
  const w = mount(AddCommandDialog);
  await w.find('.btn-save').trigger('click');
  expect(f.posts).toHaveLength(0);
  expect(w.find('.errline').text()).toContain('名称');

  await w.find('[name=name]').setValue('磁盘');
  await w.find('.btn-save').trigger('click');
  expect(f.posts).toHaveLength(0);
  expect(w.find('.errline').text()).toContain('命令');
});

it('提交用 snake_case（后端 JSON 解码拒绝未知字段，驼峰会直接 400）', async () => {
  const w = mount(AddCommandDialog);
  await w.find('[name=name]').setValue('磁盘排行');
  await w.find('[name=command]').setValue('du -sh /*');
  await w.find('[name=cwd]').setValue('/data');
  await w.find('.btn-save').trigger('click');
  await flushPromises();
  expect(f.posts[0].p).toBe('/api/commands');
  expect(f.posts[0].b).toEqual({
    name: '磁盘排行', command: 'du -sh /*', cwd: '/data', need_confirm: false,
  });
  expect(w.find('.errline').exists()).toBe(false);
  expect(w.emitted('saved')).toHaveLength(1);
});

// 确认开关默认关着是对的，但它必须真的能打开：
// 一条 `rm -rf` 无法被标记成需要确认，比多点一次勾选贵得多。
it('勾上"执行前二次确认"后按 need_confirm=true 提交', async () => {
  const w = mount(AddCommandDialog);
  await w.find('[name=name]').setValue('清缓存');
  await w.find('[name=command]').setValue('rm -rf /tmp/x');
  await w.find('[name=need_confirm]').setValue(true);
  await w.find('.btn-save').trigger('click');
  await flushPromises();
  expect(f.posts[0].b).toMatchObject({ need_confirm: true });
});

it('编辑态：回填原值并按 PATCH 提交', async () => {
  const w = mount(AddCommandDialog, { props: { editing: row({ id: 8 }) } });
  expect((w.find('[name=name]').element as HTMLInputElement).value).toBe('看盘');
  expect((w.find('[name=command]').element as HTMLTextAreaElement).value).toBe('df -h');
  await w.find('.btn-save').trigger('click');
  await flushPromises();
  expect(f.patches[0].p).toBe('/api/commands/8');
  // 编辑态交完整字段：后端 PATCH 走的是与 POST 同一套必填校验，
  // 只发"改动的字段"会被"名称必填"挡掉，报错还指着用户没动过的框。
  expect(f.patches[0].b).toEqual({
    name: '看盘', command: 'df -h', cwd: '', need_confirm: false,
  });
});

// 危险命令的判定唯一归属在后端 normalized()：表单里不许复制一套正则去
// "提示后端会强制确认"。两处规则一漂移，提示就成了谎话 —— 而这条谎话
// 只在最要紧的命令上出现。显示结论的正确位置是列表上的"确认"角标，
// 那个值来自后端。
it('表单里没有第二套危险判定：只交一个 need_confirm 字段', async () => {
  const w = mount(AddCommandDialog);
  await w.find('[name=name]').setValue('清缓存');
  await w.find('[name=command]').setValue('rm -rf /data/cache');
  await w.find('.btn-save').trigger('click');
  await flushPromises();
  // 前端原样交上去（未勾选就是 false），强制改成 true 是后端的事
  expect(f.posts[0].b).toMatchObject({ need_confirm: false });
});

it('取消发 close', async () => {
  const w = mount(AddCommandDialog);
  await w.find('.btn-cancel').trigger('click');
  expect(w.emitted('close')).toHaveLength(1);
});

it('删除要两步确认，且发出 deleted', async () => {
  const w = mount(AddCommandDialog, { props: { editing: row() } });
  await w.find('.btn-del').trigger('click');
  expect(w.emitted('deleted')).toBeUndefined(); // 第一步只武装
  await w.find('.btn-confirm-del').trigger('click');
  expect(w.emitted('deleted')).toHaveLength(1);
});

it('后端报错时显示原因，不静默关掉抽屉', async () => {
  f.api.post = vi.fn(async () => {
    throw new Error('名称已存在');
  }) as never;
  setApi(f.api as never, location);
  const w = mount(AddCommandDialog);
  await w.find('[name=name]').setValue('重复的');
  await w.find('[name=command]').setValue('ls');
  await w.find('.btn-save').trigger('click');
  await flushPromises();
  expect(w.find('.errline').text()).toContain('名称已存在');
  expect(w.emitted('saved')).toBeUndefined();
});
