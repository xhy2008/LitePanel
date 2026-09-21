// fetch 封装：统一下发 CSRF 头、解析统一错误体、401 跳登录。
export interface ApiError {
  code: string;
  message: string;
  status: number;
  detail?: string;
  // 429 时后端给的 Retry-After 秒数；登录页据此禁用提交。
  retryAfter?: number;
}

export interface LocationLike {
  pathname: string;
  search: string;
  assign(path: string): void;
}

export interface HttpDeps {
  fetch: typeof globalThis.fetch;
  location: LocationLike;
}

const LOGIN_PATH = '/login';

export function createApi(deps: HttpDeps) {
  const doFetch = deps.fetch;
  const location = deps.location;

  async function request<T>(method: string, path: string, body?: unknown): Promise<T> {
    const headers: Record<string, string> = {};
    if (method !== 'GET' && method !== 'HEAD') {
      // 后端 CSRF 防护要求（设计 5.7）。
      headers['X-Requested-With'] = 'litepanel';
      if (body !== undefined) headers['Content-Type'] = 'application/json';
    }
    const res = await doFetch(path, {
      method,
      headers,
      credentials: 'same-origin',
      body: body === undefined ? undefined : JSON.stringify(body),
    });

    if (res.status === 204) return null as T;

    let payload: any = null;
    try {
      payload = await res.json();
    } catch {
      payload = null;
    }

    if (!res.ok) {
      const err: ApiError = {
        code: payload?.code ?? 'http_error',
        message: payload?.message ?? `request failed (${res.status})`,
        status: res.status,
        detail: payload?.detail,
      };
      const retryAfter = Number(res.headers?.get?.('Retry-After'));
      if (res.status === 429 && Number.isFinite(retryAfter) && retryAfter > 0) {
        err.retryAfter = Math.round(retryAfter);
      }
      // 会话过期 → 去登录并记住来路。两种情况不跳：
      //   1. 登录接口自身的 401（密码错），跳了永远进不去；
      //   2. 已经在登录页 —— 否则 assign 会整页重载，重新挂载又发一次
      //      拿 401 的请求，浏览器就在登录页上无限刷屏频闪（实测过）。
      // 护栏必须看「当前在哪个页」，只看请求路径拦不住这一类。
      if (
        res.status === 401 &&
        !path.endsWith('/api/login') &&
        location.pathname !== LOGIN_PATH
      ) {
        const next = encodeURIComponent(location.pathname + location.search);
        location.assign(`${LOGIN_PATH}?next=${next}`);
      }
      throw err;
    }
    return payload as T;
  }

  return {
    get: <T>(path: string) => request<T>('GET', path),
    post: <T>(path: string, body?: unknown) => request<T>('POST', path, body),
    patch: <T>(path: string, body?: unknown) => request<T>('PATCH', path, body),
    put: <T>(path: string, body?: unknown) => request<T>('PUT', path, body),
    del: <T>(path: string) => request<T>('DELETE', path),
  };
}

export type Api = ReturnType<typeof createApi>;

let singleton: Api | null = null;

/** 浏览器环境下的默认实例。 */
export function api(): Api {
  if (!singleton) {
    singleton = createApi({
      fetch: globalThis.fetch.bind(globalThis),
      location: {
        get pathname() {
          return window.location.pathname;
        },
        get search() {
          return window.location.search;
        },
        assign(path: string) {
          window.location.assign(path);
        },
      },
    });
  }
  return singleton;
}
