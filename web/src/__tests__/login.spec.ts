import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { flushPromises, mount } from '@vue/test-utils';
import { createPinia, setActivePinia } from 'pinia';
import { createRouter, createMemoryHistory, type RouteRecordRaw } from 'vue-router';
import LoginView from '../views/LoginView.vue';
import { useAuthStore } from '../stores/auth';
import { setApi, resetApi } from '../api/inject';
import { createApi, type LocationLike } from '../api/http';

const Secret = { template: '<div class="secret">x</div>' };
const routes: RouteRecordRaw[] = [
  { path: '/quick', name: 'quick', component: Secret, meta: { title: '快捷命令' } },
  { path: '/', name: 'root', component: Secret, meta: { title: '首页' } },
  { path: '/login', name: 'login', component: LoginView, meta: { title: '登录', bare: true } },
];

async function boot(handler: (url: string) => Response, from = '/login') {
  const location: LocationLike = { pathname: '/login', search: '', assign: vi.fn() };
  const fetchMock = vi.fn(async (url: string) => handler(url)) as unknown as typeof globalThis.fetch;
  setApi(createApi({ fetch: fetchMock, location }), location);
  const pinia = createPinia();
  setActivePinia(pinia);
  const router = createRouter({ history: createMemoryHistory(), routes });
  await router.push(from);
  await flushPromises();
  const w = mount(LoginView, { global: { plugins: [router, pinia] } });
  return { w, router, pinia, fetchMock };
}

function json(status: number, body: unknown) {
  return new Response(JSON.stringify(body), {
    status,
    headers: { 'Content-Type': 'application/json' },
  });
}

beforeEach(() => setActivePinia(createPinia()));
afterEach(() => resetApi());

describe('登录页', () => {
  it('提交密码后跳到 next 指定的来路', async () => {
    const { w, router } = await boot(() => json(200, { ok: true, must_change_password: false }));
    await router.replace('/login?next=/quick');
    await w.get('input[name="password"]').setValue('secret-pass');
    await w.get('form').trigger('submit.prevent');
    await flushPromises();
    expect(router.currentRoute.value.path).toBe('/quick');
  });

  it('无 next 时跳首页', async () => {
    const { w, router } = await boot(() => json(200, { ok: true, must_change_password: false }));
    await w.get('input[name="password"]').setValue('secret-pass');
    await w.get('form').trigger('submit.prevent');
    await flushPromises();
    expect(router.currentRoute.value.path).toBe('/');
  });

  it('next 是站外地址时忽略（防开放重定向）', async () => {
    const { w, router } = await boot(() => json(200, { ok: true, must_change_password: false }));
    await router.replace('/login?next=//evil.example.com');
    await w.get('input[name="password"]').setValue('secret-pass');
    await w.get('form').trigger('submit.prevent');
    await flushPromises();
    expect(router.currentRoute.value.path).toBe('/');
  });

  it('密码错误：留在登录页并显示后端文案', async () => {
    const { w, router } = await boot(() => json(401, { code: 'bad_credentials', message: '密码错误' }));
    await w.get('input[name="password"]').setValue('nope');
    await w.get('form').trigger('submit.prevent');
    await flushPromises();
    expect(router.currentRoute.value.name).toBe('login');
    expect(w.get('.error').text()).toContain('密码错误');
  });

  it('被锁定时显示倒计时文案且在倒计时结束前禁用提交', async () => {
    const { w } = await boot(() => ({
      status: 429,
      ok: false,
      json: async () => ({ code: 'locked', message: '登录失败次数过多，请 10 分钟后再试' }),
      headers: new Headers({ 'Retry-After': '600' }),
    }) as Response);
    await w.get('input[name="password"]').setValue('nope');
    await w.get('form').trigger('submit.prevent');
    await flushPromises();
    expect(w.get('.error').text()).toContain('10 分钟');
    expect(w.get('button[type="submit"]').attributes('disabled')).toBeDefined();
  });

  it('倒计时归零后自动解锁，无需刷新页面', async () => {
    vi.useFakeTimers();
    const { w } = await boot(() => ({
      status: 429,
      ok: false,
      json: async () => ({ code: 'locked', message: '登录失败次数过多' }),
      headers: new Headers({ 'Retry-After': '3' }),
    }) as Response);
    await w.get('input[name="password"]').setValue('nope');
    await w.get('form').trigger('submit.prevent');
    await flushPromises();
    expect(w.get('button[type="submit"]').attributes('disabled')).toBeDefined();

    // 走到锁定截止时刻：心跳应把状态清掉并恢复可提交。
    vi.advanceTimersByTime(3100);
    await flushPromises();
    await w.vm.$nextTick();
    expect(w.get('button[type="submit"]').attributes('disabled')).toBeUndefined();
    vi.useRealTimers();
  });

  it('提交中禁用按钮并清空上一次错误（防连点重复登录）', async () => {
    let resolveFn: (r: Response) => void = () => {};
    const location: LocationLike = { pathname: '/login', search: '', assign: vi.fn() };
    const fetchMock = vi.fn(
      () => new Promise<Response>((r) => (resolveFn = r)),
    ) as unknown as typeof globalThis.fetch;
    setApi(createApi({ fetch: fetchMock, location }), location);
    const pinia = createPinia();
    setActivePinia(pinia);
    const router = createRouter({ history: createMemoryHistory(), routes });
    await router.push('/login');
    const w = mount(LoginView, { global: { plugins: [router, pinia] } });

    await w.get('input[name="password"]').setValue('x');
    await w.get('form').trigger('submit.prevent');
    await flushPromises();
    expect(w.get('button[type="submit"]').attributes('disabled')).toBeDefined();
    expect(fetchMock).toHaveBeenCalledTimes(1);

    resolveFn(json(200, { ok: true, must_change_password: false }));
    await flushPromises();
    expect(w.find('.error').exists()).toBe(false);
  });

  it('空密码不发包', async () => {
    const { w, fetchMock } = await boot(() => json(200, { ok: true }));
    await w.get('form').trigger('submit.prevent');
    await flushPromises();
    expect(fetchMock).not.toHaveBeenCalled();
  });

  it('页面上有面板标识，便于确认不是别的服务占用了端口', async () => {
    const { w } = await boot(() => json(200, { ok: true }));
    expect(w.text()).toContain('LitePanel');
  });
});
