import type { Router } from 'vue-router';
import { useAuthStore } from '../stores/auth';

// 登录守卫：进任意受保护页前先问一次 /api/me。
// 未登录 → /login?next=...；已登录访问 /login → 回首页。
export function installAuthGuard(router: Router) {
  let booted = false;

  router.beforeEach(async (to) => {
    const auth = useAuthStore();
    if (!booted) {
      booted = true;
      try {
        await auth.fetchMe();
      } catch {
        // /api/me 拉取失败不应该把人永久锁在门外；
        // 放行后真实请求会 401，由 http.ts 统一跳登录。
        return true;
      }
    }

    if (to.meta?.bare) {
      // 已登录就没必要再看登录页。
      return auth.loggedIn ? { name: 'quick' } : true;
    }
    if (!auth.loggedIn) {
      return { name: 'login', query: { next: to.fullPath } };
    }
    return true;
  });
}
