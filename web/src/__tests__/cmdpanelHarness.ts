import { createRouter, createMemoryHistory, type RouteRecordRaw } from 'vue-router';

const Blank = { template: '<div />' };
const routes = [
  { path: '/quick', name: 'quick', component: Blank },
  { path: '/term', name: 'term', component: Blank },
] as RouteRecordRaw[];

// 只注册被测代码真正会去的那个页。写成通配或缺 /term 的话，
// "跳到终端页"这条断言会在路由失败的情况下照样绿（router.push 抛的错
// 被 await 吞掉后看不见），而线上是跳不过去。
export function makeRouter() {
  return createRouter({ history: createMemoryHistory(), routes });
}
