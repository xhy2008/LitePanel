import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { flushPromises, mount } from '@vue/test-utils';
import { createPinia, setActivePinia } from 'pinia';
import { createRouter, createMemoryHistory, type RouteRecordRaw } from 'vue-router';
import App from '../App.vue';
import { installAuthGuard } from '../router/guard';
import { setApi, resetApi } from '../api/inject';
import { createApi, type LocationLike } from '../api/http';
import { useAuthStore } from '../stores/auth';

const LoginStub = { template: '<div class="login-page">登录</div>' };
const Secret = { template: '<div class="secret">受保护内容</div>' };

const routes: RouteRecordRaw[] = [
  { path: '/', redirect: '/quick' },
  { path: '/quick', name: 'quick', component: Secret, meta: { title: '快捷命令' } },
  { path: '/login', name: 'login', component: LoginStub, meta: { title: '登录', bare: true } },
];

function apiReturning(handler: (url: string) => Response) {
  const location: LocationLike = { pathname: '/', search: '', assign: vi.fn() };
  const fetchMock = vi.fn(async (url: string) => handler(url)) as unknown as typeof globalThis.fetch;
  setApi(createApi({ fetch: fetchMock, location }), location);
}

async function boot(me: Response) {
  apiReturning(() => me);
  // 守卫（组件外）与组件必须共用同一个 pinia 实例，否则是两份状态。
  const pinia = createPinia();
  setActivePinia(pinia);
  const router = createRouter({ history: createMemoryHistory(), routes });
  installAuthGuard(router);
  await router.push('/quick');
  await flushPromises();
  const w = mount(App, { global: { plugins: [router, pinia] } });
  await flushPromises();
  return { w, router };
}

beforeEach(() => setActivePinia(createPinia()));
afterEach(() => {
  resetApi();
  setActivePinia(createPinia());
});

describe('登录守卫', () => {
  it('未登录访问受保护页面被重定向到 /login，并带上 next', async () => {
    const { router } = await boot(
      new Response(JSON.stringify({ authenticated: false, must_change_password: false }), {
        headers: { 'Content-Type': 'application/json' },
      }),
    );
    expect(router.currentRoute.value.name).toBe('login');
    expect(router.currentRoute.value.query.next).toBe('/quick');
  });

  it('已登录不再重定向', async () => {
    const { w, router } = await boot(
      new Response(JSON.stringify({ authenticated: true, must_change_password: false }), {
        headers: { 'Content-Type': 'application/json' },
      }),
    );
    expect(router.currentRoute.value.name).toBe('quick');
    expect(w.find('.secret').exists()).toBe(true);
  });

  it('已登录但需改密：停在改密界面，不渲染业务内容', async () => {
    const { w } = await boot(
      new Response(JSON.stringify({ authenticated: true, must_change_password: true }), {
        headers: { 'Content-Type': 'application/json' },
      }),
    );
    expect(w.find('.force-password').exists()).toBe(true);
    expect(w.find('.secret').exists()).toBe(false);
  });

  it('登录页本身不被守卫挡（否则会死循环）', async () => {
    apiReturning(() =>
      new Response(JSON.stringify({ authenticated: false, must_change_password: false }), {
        headers: { 'Content-Type': 'application/json' },
      }),
    );
    setActivePinia(createPinia());
    const router = createRouter({ history: createMemoryHistory(), routes });
    installAuthGuard(router);
    await router.push('/login');
    await flushPromises();
    expect(router.currentRoute.value.name).toBe('login');
  });

  it('已登录访问 /login 直接送回首页', async () => {
    apiReturning(() =>
      new Response(JSON.stringify({ authenticated: true, must_change_password: false }), {
        headers: { 'Content-Type': 'application/json' },
      }),
    );
    setActivePinia(createPinia());
    const router = createRouter({ history: createMemoryHistory(), routes });
    installAuthGuard(router);
    await router.push('/login');
    await flushPromises();
    expect(router.currentRoute.value.name).toBe('quick');
  });

  it('me() 失败时不把用户锁在门外（放行，后续由后端 401 兜底）', async () => {
    apiReturning(() => new Response('{}', { status: 500 }));
    setActivePinia(createPinia());
    const router = createRouter({ history: createMemoryHistory(), routes });
    installAuthGuard(router);
    await router.push('/quick');
    await flushPromises();
    expect(router.currentRoute.value.name).toBe('quick');
  });
});

describe('改密界面', () => {
  it('提交后调用 changePassword 并提示需重新登录', async () => {
    apiReturning(() =>
      new Response(JSON.stringify({ ok: true, reauth_required: true }), {
        headers: { 'Content-Type': 'application/json' },
      }),
    );
    setActivePinia(createPinia());
    const auth = useAuthStore();
    auth.$patch({ loggedIn: true, mustChangePassword: true });

    const { default: ForcePassword } = await import('../views/ForcePassword.vue');
    const w = mount(ForcePassword);
    await w.get('input[name="old"]').setValue('initial123');
    await w.get('input[name="new"]').setValue('brand-new-pass');
    await w.get('input[name="confirm"]').setValue('brand-new-pass');
    await w.get('form').trigger('submit.prevent');
    await flushPromises();

    expect(w.find('.reauth-hint').exists()).toBe(true);
    expect(auth.loggedIn).toBe(false);
  });

  it('两次输入不一致时前端就拦住，不打无谓的请求', async () => {
    const fetchMock = vi.fn() as unknown as typeof globalThis.fetch;
    setApi(createApi({ fetch: fetchMock, location: { pathname: '/', search: '', assign: vi.fn() } }), {
      pathname: '/',
      search: '',
      assign: vi.fn(),
    });
    setActivePinia(createPinia());
    useAuthStore().$patch({ loggedIn: true, mustChangePassword: true });

    const { default: ForcePassword } = await import('../views/ForcePassword.vue');
    const w = mount(ForcePassword);
    await w.get('input[name="old"]').setValue('initial123');
    await w.get('input[name="new"]').setValue('brand-new-pass');
    await w.get('input[name="confirm"]').setValue('different-pass');
    await w.get('form').trigger('submit.prevent');
    await flushPromises();

    expect(fetchMock).not.toHaveBeenCalled();
    expect(w.find('.mismatch').exists()).toBe(true);
  });
});
