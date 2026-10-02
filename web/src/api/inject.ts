import type { Api, LocationLike } from './http';

/** 可注入的 fetch。上传分块要走它（api 的封装只发 JSON，见 api/http.ts）。 */
export type Fetcher = typeof globalThis.fetch;

// api 实例的注入点：应用启动时装配真实实现，测试里换成替身。
// store 通过它拿到 api，避免在 store 里直接 new 出不可替换的依赖。
//
// fetch 一并挂在这里而不是让 store 自己去 import：多一个注入入口就多一个
// "测试里忘了注入、于是打了真网络"的地方。
let holder: { api: Api; location: LocationLike; fetch: Fetcher } | null = null;

export function setApi(api: Api, location: LocationLike, fetch?: Fetcher) {
  holder = { api, location, fetch: fetch ?? globalThis.fetch.bind(globalThis) };
}

export function getApi(): { api: Api; location: LocationLike; fetch: Fetcher } {
  if (!holder) {
    throw new Error('api 未注入：请在 main.ts 里先调用 setApi()');
  }
  return holder;
}

export function resetApi() {
  holder = null;
}
