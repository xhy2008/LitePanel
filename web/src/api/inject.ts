import type { Api, LocationLike } from './http';

// api 实例的注入点：应用启动时装配真实实现，测试里换成替身。
// store 通过它拿到 api，避免在 store 里直接 new 出不可替换的依赖。
let holder: { api: Api; location: LocationLike } | null = null;

export function setApi(api: Api, location: LocationLike) {
  holder = { api, location };
}

export function getApi(): { api: Api; location: LocationLike } {
  if (!holder) {
    throw new Error('api 未注入：请在 main.ts 里先调用 setApi()');
  }
  return holder;
}

export function resetApi() {
  holder = null;
}
