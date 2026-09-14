import { createRouter, createWebHistory, type RouteRecordRaw } from 'vue-router';
import AppShell from '../layout/AppShell.vue';

// 五个一级 tab + 登录页。各 tab 一律渲染，不做 display:none 技巧。
const routes: RouteRecordRaw[] = [
  {
    path: '/',
    component: AppShell,
    children: [
      { path: '', redirect: { name: 'quick' } },
      { path: 'quick', name: 'quick', component: () => import('../views/Quick.vue'), meta: { title: '快捷命令' } },
      { path: 'term', name: 'term', component: () => import('../views/Terminal.vue'), meta: { title: '终端' } },
      { path: 'files', name: 'files', component: () => import('../views/Files.vue'), meta: { title: '文件管理' } },
      { path: 'downloads', name: 'downloads', component: () => import('../views/Downloads.vue'), meta: { title: '下载' } },
      { path: 'settings', name: 'settings', component: () => import('../views/Settings.vue'), meta: { title: '设置' } },
    ],
  },
  { path: '/login', name: 'login', component: () => import('../views/Login.vue'), meta: { title: '登录' } },
];

export function makeRouter() {
  const r = createRouter({ history: createWebHistory(), routes });
  r.afterEach((to) => {
    const t = to.meta?.title as string | undefined;
    if (t) document.title = `${t} · LitePanel`;
  });
  return r;
}

export { routes };
