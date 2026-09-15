import { createRouter, createWebHistory, type RouteRecordRaw } from 'vue-router';
import { installAuthGuard } from './guard';

// 五个一级 tab + 登录页。布局外壳在 App.vue（断点分支），
// 因此父路由不带 component，子视图直接渲染进 App.vue 的出口。
const routes: RouteRecordRaw[] = [
  {
    path: '/',
    children: [
      { path: '', redirect: { name: 'quick' } },
      { path: 'quick', name: 'quick', component: () => import('../views/QuickCmdView.vue'), meta: { title: '快捷命令' } },
      { path: 'term', name: 'term', component: () => import('../views/TerminalView.vue'), meta: { title: '终端' } },
      { path: 'files', name: 'files', component: () => import('../views/FilesView.vue'), meta: { title: '文件管理' } },
      { path: 'downloads', name: 'downloads', component: () => import('../views/DownloadsView.vue'), meta: { title: '下载' } },
      { path: 'settings', name: 'settings', component: () => import('../views/SettingsView.vue'), meta: { title: '设置' } },
    ],
  },
      // bare 必须有：守卫靠它识别“登录页自身”，否则会把它当受保护页
    // 再往 /login 跳，形成重定向自循环并冻结主线程（实测整页卡死）。
    { path: '/login', name: 'login', component: () => import('../views/LoginView.vue'), meta: { title: '登录', bare: true } },
];

export function makeRouter() {
  const r = createRouter({ history: createWebHistory(), routes });
  installAuthGuard(r);
  r.afterEach((to) => {
    const t = to.meta?.title as string | undefined;
    if (t) document.title = `${t} · LitePanel`;
  });
  return r;
}

export { routes };
