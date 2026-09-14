import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { createApi } from '../api/http';

// http.ts 契约：写方法带 CSRF 头、401 跳登录并携带 next、错误体统一解析。
describe('api/http', () => {
  let fetchMock: ReturnType<typeof vi.fn>;
  const redirects: string[] = [];
  const location = { pathname: '/services', search: '', assign(path: string) { redirects.push(path); } };

  beforeEach(() => {
    redirects.length = 0;
    fetchMock = vi.fn();
    vi.stubGlobal('fetch', fetchMock);
  });
  afterEach(() => vi.unstubAllGlobals());

  function respond(status: number, body: unknown) {
    fetchMock.mockResolvedValue({
      status,
      ok: status >= 200 && status < 300,
      json: async () => body,
      headers: new Headers(),
    });
  }

  it('GET 不带 CSRF 头', async () => {
    respond(200, { ok: true });
    const api = createApi({ fetch: fetchMock, location });
    await api.get('/api/settings');
    const [, init] = fetchMock.mock.calls[0];
    expect(init.method).toBe('GET');
    expect(init.headers?.['X-Requested-With']).toBeUndefined();
  });

  it('POST 带 X-Requested-With: litepanel', async () => {
    respond(200, { ok: true });
    const api = createApi({ fetch: fetchMock, location });
    await api.post('/api/login', { password: 'x' });
    const [, init] = fetchMock.mock.calls[0];
    expect(init.method).toBe('POST');
    expect(init.headers?.['X-Requested-With']).toBe('litepanel');
    expect(init.headers?.['Content-Type']).toBe('application/json');
    expect(init.body).toBe(JSON.stringify({ password: 'x' }));
    expect(init.credentials).toBe('same-origin');
  });

  it('401 时跳转登录并携带回来时的路径', async () => {
    respond(401, { code: 'unauthorized', message: '未登录' });
    const api = createApi({ fetch: fetchMock, location });
    await expect(api.get('/api/settings')).rejects.toMatchObject({ code: 'unauthorized' });
    expect(redirects).toHaveLength(1);
    expect(redirects[0]).toContain('/login');
    expect(redirects[0]).toContain(encodeURIComponent('/services'));
  });

  it('登录接口自身 401 不跳登录（否则永远进不去）', async () => {
    respond(401, { code: 'bad_credentials', message: '密码错误' });
    const api = createApi({ fetch: fetchMock, location });
    await expect(api.post('/api/login', { password: 'x' })).rejects.toMatchObject({
      code: 'bad_credentials',
    });
    expect(redirects).toHaveLength(0);
  });

  it('非 2xx 抛出统一错误结构', async () => {
    respond(403, { code: 'csrf', message: '缺少或错误的 X-Requested-With 头', detail: '' });
    const api = createApi({ fetch: fetchMock, location });
    await expect(api.post('/api/logout')).rejects.toMatchObject({
      code: 'csrf',
      status: 403,
    });
  });

  it('429 时把 Retry-After 秒数带进错误对象（登录页据此禁用提交）', async () => {
    fetchMock.mockResolvedValue({
      status: 429,
      ok: false,
      json: async () => ({ code: 'locked', message: '稍后再试' }),
      headers: new Headers({ 'Retry-After': '600' }),
    });
    const api = createApi({ fetch: fetchMock, location });
    await expect(api.post('/api/login', { password: 'x' })).rejects.toMatchObject({
      code: 'locked',
      retryAfter: 600,
    });
  });

  it('204 无体响应返回 null', async () => {
    fetchMock.mockResolvedValue({ status: 204, ok: true, json: async () => ({}) });
    const api = createApi({ fetch: fetchMock, location });
    await expect(api.post('/api/x')).resolves.toBeNull();
  });
});
