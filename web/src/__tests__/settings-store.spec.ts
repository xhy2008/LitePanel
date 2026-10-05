import { describe, it, expect, vi, beforeEach } from 'vitest';
import { setActivePinia, createPinia } from 'pinia';
import { setApi, resetApi } from '../api/inject';
import { useSettingsStore } from '../stores/settings';
import type { SettingItem, SettingGroup } from '../api/settings';

// 与后端 settingItem 的 JSON tag 对齐（前端不做 camelCase 转换）。
function item(over: Partial<SettingItem> = {}): SettingItem {
  return {
    key: 'login_max_fails',
    group: 'auth',
    kind: 'int',
    label: '登录失败锁定阈值',
    unit: '次',
    min: 3,
    max: 100,
    value: 5,
    valid: true,
    set: true,
    overridden: false,
    ...over,
  };
}

function toGroups(items: SettingItem[]): SettingGroup[] {
  const by = new Map<string, SettingItem[]>();
  for (const it of items) {
    by.set(it.group, [...(by.get(it.group) ?? []), it]);
  }
  return [...by.entries()].map(([group, gitems]) => ({ group, items: gitems }));
}

// 记录每次 PUT 的请求体，供断言"只发改过的项"。
let lastPut: Record<string, unknown> | null = null;
let putResp: unknown = { ok: true, applied: true, groups: [] };

function boot(items: SettingItem[], opts: { putFails?: unknown } = {}) {
  setActivePinia(createPinia());
  lastPut = null;
  const api = {
    get: vi.fn(async () => ({ groups: toGroups(items) })),
    put: vi.fn(async (_p: string, body: Record<string, unknown>) => {
      // putFails 非空时模拟保存报错（必须在这里抛，而不是在测试里 spread
      // 一个新对象 —— store 通过 getApi() 拿的是 boot 里 setApi 注入的那一个,
      // 对拷贝对象改 put 根本不会生效，测试会假绿）。
      if (opts.putFails) throw opts.putFails;
      lastPut = body;
      return putResp;
    }),
    post: vi.fn(async () => ({ ok: true, reauth_required: true })),
    patch: vi.fn(async () => ({})),
    del: vi.fn(async () => ({})),
  };
  setApi(
    api as never,
    { pathname: '/', search: '', assign() {} } as never,
  );
  return api;
}

beforeEach(() => {
  resetApi();
  putResp = { ok: true, applied: true, groups: [] };
});

describe('settings store', () => {
  it('load 填充 items 并清 dirty', async () => {
    boot([item(), item({ key: 'session_ttl_days', group: 'auth', value: 30 })]);
    const s = useSettingsStore();
    await s.load();
    expect(s.items).toHaveLength(2);
    expect(s.loaded).toBe(true);
    expect(s.dirtyCount).toBe(0);
  });

  it('edit 标记 dirty；改回原值自动撤销', async () => {
    boot([item()]);
    const s = useSettingsStore();
    await s.load();
    s.edit('login_max_fails', 9);
    expect(s.dirtyCount).toBe(1);
    expect(s.display('login_max_fails')).toBe(9);
    // 改回服务器值：草稿标记应自动撤掉（否则保存按钮灰不掉）。
    s.edit('login_max_fails', 5);
    expect(s.dirtyCount).toBe(0);
  });

  it('save 只发改过的项，不发全量', async () => {
    putResp = { ok: true, applied: true, groups: toGroups([item({ value: 9 })]) };
    boot([item(), item({ key: 'session_ttl_days...', value: 30 })]);
    const s = useSettingsStore();
    await s.load();
    s.edit('login_max_fails', 9);
    await s.save();
    expect(lastPut).toEqual({ login_max_fails: 9 });
  });

  it('save 成功后用返回的 groups 覆盖本地并清 dirty', async () => {
    putResp = {
      ok: true,
      applied: true,
      groups: toGroups([item({ value: 9, overridden: true })]),
    };
    boot([item()]);
    const s = useSettingsStore();
    await s.load();
    s.edit('login_max_fails', 9);
    await s.save();
    expect(s.dirtyCount).toBe(0);
    expect(s.items[0].value).toBe(9);
    expect(s.items[0].overridden).toBe(true);
  });

  it('applied:false 时提示原样带原因（不许吞掉静默不生效）', async () => {
    putResp = {
      ok: true,
      applied: false,
      reason: 'aria2 拒绝了选项',
      groups: toGroups([item()]),
    };
    boot([item()]);
    const s = useSettingsStore();
    await s.load();
    s.edit('login_max_fails', 7);
    await s.save();
    expect(s.notice).toContain('aria2 拒绝了选项');
  });

  it('改动含需重启项时，提示里补一句需重启', async () => {
    putResp = {
      ok: true,
      applied: true,
      groups: toGroups([item({ key: 'aria2_rpc_url', restart_required: true })]),
    };
    boot([item({ key: 'aria2_rpc_url', kind: 'string', restart_required: true })]);
    const s = useSettingsStore();
    await s.load();
    s.edit('aria2_rpc_url', 'http://127.0.0.1:6800/jsonrpc');
    await s.save();
    expect(s.notice).toContain('重启');
  });

  it('无改动时 save 不发请求', async () => {
    const api = boot([item()]);
    const s = useSettingsStore();
    await s.load();
    const ok = await s.save();
    expect(ok).toBe(false);
    expect(api.put).not.toHaveBeenCalled();
  });

  it('save 失败时保留 dirty（让用户能重试，不吞改动）', async () => {
    const api = boot([item()], { putFails: { message: '后端 500' } });
    const s = useSettingsStore();
    await s.load();
    s.edit('login_max_fails', 8);
    const ok = await s.save();
    expect(ok).toBe(false);
    expect(s.error).toContain('后端 500');
    // 关键：改动还在，用户能原样重试而不是白填一遍。
    expect(s.dirtyCount).toBe(1);
    expect(api.put).toHaveBeenCalledTimes(1);
  });

  it('groups 按已知分组顺序排，未知分组排最后且不会消失', async () => {
    boot([
      item({ key: 'aria2_split', group: 'download' }),
      item({ key: 'login_max_fails', group: 'auth' }),
      item({ key: 'brand_new', group: 'zzz_future' }),
    ]);
    const s = useSettingsStore();
    await s.load();
    const names = s.groups.map((g) => g.group);
    expect(names).toEqual(['auth', 'download', 'zzz_future']);
  });
});
