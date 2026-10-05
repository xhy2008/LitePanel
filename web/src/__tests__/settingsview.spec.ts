import { beforeEach, describe, expect, it, vi } from 'vitest';
import { flushPromises, mount } from '@vue/test-utils';
import { createPinia, setActivePinia } from 'pinia';
import { createRouter, createMemoryHistory } from 'vue-router';
import SettingsView from '../views/SettingsView.vue';
import { setApi, resetApi } from '../api/inject';
import type { SettingItem, SettingGroup } from '../api/settings';

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
  for (const it of items) by.set(it.group, [...(by.get(it.group) ?? []), it]);
  return [...by.entries()].map(([group, g]) => ({ group, items: g }));
}

let putResp: unknown = { ok: true, applied: true, groups: [] };
let lastPut: Record<string, unknown> | null = null;

function mkRouter() {
  return createRouter({
    history: createMemoryHistory(),
    routes: [
      { path: '/', name: 'settings', component: SettingsView },
      { path: '/login', name: 'login', component: { template: '<div/>' } },
    ],
  });
}

function boot(items: SettingItem[]) {
  const pinia = createPinia();
  setActivePinia(pinia);
  lastPut = null;
  const api = {
    get: vi.fn(async () => ({ groups: toGroups(items) })),
    put: vi.fn(async (_p: string, b: Record<string, unknown>) => {
      lastPut = b;
      return putResp;
    }),
    post: vi.fn(async () => ({ ok: true, reauth_required: true })),
    patch: vi.fn(async () => ({})),
    del: vi.fn(async () => ({})),
  };
  setApi(api as never, { pathname: '/', search: '', assign() {} } as never);
  return { api, pinia };
}

async function render(items: SettingItem[]) {
  const { pinia } = boot(items);
  const router = mkRouter();
  await router.push('/');
  const w = mount(SettingsView, { global: { plugins: [pinia, router] } });
  await flushPromises();
  return w;
}

beforeEach(() => {
  resetApi();
  putResp = { ok: true, applied: true, groups: [] };
});

describe('SettingsView', () => {
  it('渲染每项的标签，int 项是 number 输入且带上后端给的 min/max', async () => {
    const w = await render([item()]);
    expect(w.text()).toContain('登录失败锁定阈值');
    const input = w.find('#f-login_max_fails');
    expect(input.attributes('type')).toBe('number');
    expect(input.attributes('min')).toBe('3');
    expect(input.attributes('max')).toBe('100');
  });

  it('改一项后出现保存条并显示改动数，未改动时不出现', async () => {
    const w = await render([item()]);
    expect(w.find('.st-bar').exists()).toBe(false);
    await w.find('#f-login_max_fails').setValue('9');
    expect(w.find('.st-bar').exists()).toBe(true);
    expect(w.find('.st-bar').text()).toContain('1 项未保存');
  });

  it('保存只 PUT 改过的项', async () => {
    putResp = { ok: true, applied: true, groups: toGroups([item({ value: 9 })]) };
    const w = await render([
      item(),
      item({ key: 'session_ttl_days', label: '会话有效期', value: 30 }),
    ]);
    await w.find('#f-login_max_fails').setValue('9');
    await w.find('.st-btn.primary').trigger('click');
    await flushPromises();
    expect(lastPut).toEqual({ login_max_fails: 9 });
  });

  it('枚举项渲染成下拉，选项来自后端的 enum', async () => {
    const w = await render([
      item({ key: 'metric_interval_sec', kind: 'enum', enum: [1, 2, 5], unit: '秒', value: 2 }),
    ]);
    const sel = w.find('#f-metric_interval_sec');
    expect(sel.element.tagName).toBe('SELECT');
    const opts = sel.findAll('option').map((o) => o.text());
    expect(opts).toEqual(['1 秒', '2 秒', '5 秒']);
  });

  it('密钥项是 password 输入，占位区分"已设置/未设置"，且从不回显原值', async () => {
    const w = await render([
      item({ key: 'aria2_rpc_secret', kind: 'string', secret: true, has_value: true, value: '' }),
    ]);
    const inp = w.find('#f-aria2_rpc_secret');
    expect(inp.attributes('type')).toBe('password');
    expect(inp.attributes('placeholder')).toContain('留空则不修改');
    expect((inp.element as HTMLInputElement).value).toBe('');
  });

  it('需重启项带"需重启"徽标', async () => {
    const w = await render([item({ key: 'aria2_rpc_url', restart_required: true })]);
    expect(w.text()).toContain('需重启');
  });

  it('库里坏值（valid:false）显示"当前值不合法"', async () => {
    const w = await render([item({ value: 'o3', valid: false })]);
    expect(w.text()).toContain('不合法');
  });

  it('applied:false 的提示原样显示后端原因', async () => {
    putResp = { ok: true, applied: false, reason: 'aria2 拒绝了选项', groups: toGroups([item()]) };
    const w = await render([item()]);
    await w.find('#f-login_max_fails').setValue('7');
    await w.find('.st-btn.primary').trigger('click');
    await flushPromises();
    expect(w.find('.st-notice').text()).toContain('aria2 拒绝了选项');
  });

  it('未知改动前保存条上的撤销能清掉 dirty', async () => {
    const w = await render([item()]);
    await w.find('#f-login_max_fails').setValue('9');
    expect(w.find('.st-bar').exists()).toBe(true);
    await w.find('.st-revert').trigger('click');
    expect(w.find('.st-bar').exists()).toBe(false);
  });
});
