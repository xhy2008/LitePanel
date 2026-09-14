import { createApp } from 'vue';
import { pinia } from './pinia';
import App from './App.vue';
import { makeRouter } from './router';
import './styles/tokens.css';
import './styles/base.css';
import { setApi } from './api/inject';
import { api as makeApi } from './api/http';
import { mountApp } from './boot';

function bootstrap() {
  const app = createApp(App);
  app.use(pinia);
  // 先注入 api，再装路由：守卫在首次导航时就要用它拉 /api/me。
  setApi(makeApi(), {
    get pathname() {
      return window.location.pathname;
    },
    get search() {
      return window.location.search;
    },
    assign(path: string) {
      window.location.assign(path);
    },
  });
  app.use(makeRouter());
  app.mount('#app');
  // 告知内联看门狗：启动真正走完了，不要再报挂起。
  window.__litepanelReady = true;
}

mountApp({
  boot: bootstrap,
  onError: (e) => {
    // 同时报给 window.onerror：内联钩子已把内容画到页面上，这里留扩展点。
    console.error('[litepanel] 启动失败', e);
  },
});
