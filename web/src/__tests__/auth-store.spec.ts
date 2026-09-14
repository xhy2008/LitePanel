import { beforeEach, describe, expect, it, vi } from 'vitest';
import { createPinia, setActivePinia } from 'pinia';
import { createApi, type LocationLike } from '../api/http';
import { resetApi, setApi } from '../api/inject';
import { useAuthStore } from '../stores/auth';

// auth store：登录态、must_change_password、以及 401 场景下的清理。
function makeApi(handler: (url: string, init: RequestInit) => Response) {
  const location: LocationLike = {
    pathname: '/services',
    search: '',
    assign: vi.fn(),
  };
  const fetchMock = vi.fn(async (url: string, init: RequestInit = {}) =>
    handler(url, init),
  ) as unknown as typeof globalThis.fetch;
  const api = createApi({ fetch: fetchMock, location });
  return { location, api, fetchMock };
}

function jsonResponse(status: number, body: unknown) {
  return new Response(JSON.stringify(body), {
    status,
    headers: { 'Content-Type': 'application/json' },
  });
}

function authWith(handler: (url: string, init: RequestInit) => Response) {
  const { api, location } = makeApi(handler);
  // store 从注入点取 api，这里换成替身。
  setApi(api, location);
  setActivePinia(createPinia());
  return useAuthStore();
}

beforeEach(() => {
  resetApi();
  setActivePinia(createPinia());
});

describe('auth store', () => {
  it('登录成功后保存 must_change_password', async () => {
    const auth = authWith(() =>
      jsonResponse(200, { ok: true, must_change_password: true }),
    );
    await auth.login('pw');
    expect(auth.loggedIn).toBe(true);
    expect(auth.mustChangePassword).toBe(true);
  });

  it('改密成功后清除 must_change_password 并要求重新登录', async () => {
    const calls: string[] = [];
    const auth = authWith((url) => {
      calls.push(url);
      return jsonResponse(200, { ok: true, reauth_required: true });
    });
    auth.$patch({ loggedIn: true, mustChangePassword: true });

    await auth.changePassword('old', 'new-pass-123');
    expect(calls).toContain('/api/password');
    // 后端吊销了全部会话，本地状态必须同步清掉，否则界面以为还登录着。
    expect(auth.loggedIn).toBe(false);
    expect(auth.mustChangePassword).toBe(false);
  });

  it('登录失败：loggedIn 不翻转，并保留可读错误', async () => {
    const auth = authWith(() =>
      jsonResponse(401, { code: 'bad_credentials', message: '密码错误' }),
    );
    await expect(auth.login('wrong')).rejects.toBeTruthy();
    expect(auth.loggedIn).toBe(false);
    expect(auth.error).toContain('密码错误');
  });

  it('被锁定时错误里带 Retry-After 语义', async () => {
    const auth = authWith(() =>
      jsonResponse(429, { code: 'locked', message: '登录失败次数过多，请 10 分钟后再试' }),
    );
    await expect(auth.login('wrong')).rejects.toBeTruthy();
    expect(auth.error).toContain('10 分钟');
    expect(auth.loggedIn).toBe(false);
  });

  it('429 时把 Retry-After 换算成 lockedUntil，供界面禁用提交', async () => {
    vi.useFakeTimers();
    vi.setSystemTime(new Date('2026-09-14T00:00:00Z'));
    const auth = authWith(() => {
      return new Response(JSON.stringify({ code: 'locked', message: '请 10 分钟后再试' }), {
        status: 429,
        headers: { 'Content-Type': 'application/json', 'Retry-After': '600' },
      });
    });
    await expect(auth.login('x')).rejects.toBeTruthy();
    expect(auth.lockedUntil).toBe(Date.now() + 600_000);
    vi.useRealTimers();
  });

  it('me() 拉取认证状态', async () => {
    const auth = authWith(() =>
      jsonResponse(200, { authenticated: true, must_change_password: false }),
    );
    await auth.fetchMe();
    expect(auth.loggedIn).toBe(true);
    expect(auth.mustChangePassword).toBe(false);
  });

  it('未登录时 me() 会把状态清零', async () => {
    const auth = authWith(() =>
      jsonResponse(200, { authenticated: false, must_change_password: true }),
    );
    auth.$patch({ loggedIn: true, mustChangePassword: true });
    await auth.fetchMe();
    expect(auth.loggedIn).toBe(false);
  });

  it('登出后状态清零，且失败也清（避免卡在半死状态）', async () => {
    const auth = authWith(() => jsonResponse(500, { code: 'internal', message: 'x' }));
    auth.$patch({ loggedIn: true, mustChangePassword: true });
    await auth.logout();
    expect(auth.loggedIn).toBe(false);
    expect(auth.mustChangePassword).toBe(false);
  });
});
