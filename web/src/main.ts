import { createApp } from 'vue';
import { pinia } from './pinia';
import App from './App.vue';
import { makeRouter } from './router';
import './styles/tokens.css';
import './styles/base.css';
import { setApi } from './api/inject';
import { api as makeApi } from './api/http';

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
