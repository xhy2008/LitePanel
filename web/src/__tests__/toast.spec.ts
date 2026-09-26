import { beforeEach, describe, expect, it, vi } from 'vitest';
import { flushPromises, mount } from '@vue/test-utils';
import { createRouter, createMemoryHistory, type RouteRecordRaw } from 'vue-router';
import { createPinia, setActivePinia } from 'pinia';
import App from '../App.vue';
import { useQuickCmdStore } from '../stores/quickcmd';
import { useTerminalStore } from '../stores/terminal';

const Blank = { template: '<div class="view" />' };
const routes: RouteRecordRaw[] = [
  { path: '/', redirect: '/quick' },
  { path: '/quick', name: 'quick', component: Blank },
  { path: '/term', name: 'term', component: Blank },
  { path: '/login', name: 'login', component: Blank, meta: { bare: true } },
];

async function mountApp(path = '/quick') {
  setActivePinia(createPinia());
  const router = createRouter({ history: createMemoryHistory(), routes });
  await router.push(path);
  await router.isReady();
  const w = mount(App, { global: { plugins: [router, createPinia()] }, attachTo: document.body });
  await flushPromises();
  return { w, router };
}

// toast 挂在壳层而不是任何一个页面里，理由不是"顺手"：
// 点快捷命令会跳终端页，那个页面在跳转中被卸载 —— 谁在页面里显示
// notice，notice 就跟着谁一起没了，用户在终端页里什么都看不见。
describe('壳层 toast', () => {
  beforeEach(() => {
    vi.useFakeTimers({ toFake: ['setTimeout', 'clearTimeout'] });
  });

  it('quickcmd 的 notice 显示出来，并被立刻取走（同一句话不会第二次不显示）', async () => {
    const { w } = await mountApp();
    const qc = useQuickCmdStore();
    qc.notice = '已新建会话 lp-4 执行';
    await flushPromises();
    expect(w.find('.toast').text()).toContain('lp-4');
    expect(qc.notice).toBe('');

  });

  // 每次都一样的文案最容易踩空：notice 若不被取走，值不变就不会再触发
  // watch，第二次点命令就毫无反应 —— 而用户正因为看不见才连点。
  // 这条必须"等第一条消失之后再放同一条"，否则测的是覆盖而不是重新触发。
  it('同一条提示连续两次，两次都要显示', async () => {
    const { w } = await mountApp();
    const qc = useQuickCmdStore();
    qc.notice = '当前会话都在忙，已新建会话 lp-4 执行';
    await flushPromises();
    expect(w.find('.toast').exists()).toBe(true);
    vi.advanceTimersByTime(4000);
    await flushPromises();
    expect(w.find('.toast').exists()).toBe(false);

    qc.notice = '当前会话都在忙，已新建会话 lp-4 执行';
    await flushPromises();
    expect(w.find('.toast').exists()).toBe(true);
  });

  // 终端 store 也要能喊话：会话在 tmux 里 exit 之后标签自己消失了，
  // 不说一句"为什么少了个标签"就成了灵异事件。
  it('terminal 的 notice 也显示（会话退出的解释）', async () => {
    const { w } = await mountApp('/term');
    useTerminalStore().notice = '会话已退出：跑任务';
    await flushPromises();
    expect(w.find('.toast').text()).toContain('跑任务');
    expect(useTerminalStore().notice).toBe('');
  });

  // 跳页之后还在：这是把 toast 放壳层的全部理由。
  it('从命令页跳到终端页之后，toast 仍在', async () => {
    const { w, router } = await mountApp();
    useQuickCmdStore().notice = '已新建会话 lp-9 执行';
    await flushPromises();
    await router.push({ name: 'term' });
    await flushPromises();
    expect(w.find('.toast').text()).toContain('lp-9');
  });

  it('到点自己消失：常驻的提示会被读成"当前状态"', async () => {
    const { w } = await mountApp();
    useQuickCmdStore().notice = '一闪而过';
    await flushPromises();
    expect(w.find('.toast').exists()).toBe(true);
    vi.advanceTimersByTime(4000);
    await flushPromises();
    expect(w.find('.toast').exists()).toBe(false);
  });

  // 裸页（登录页）不套外壳，自然也没有 toast。注意：只断言".toast 不在"
  // 是条空话 —— 这个组件不挂 App 也能过。所以同时要求 notice 原封不动
  // 留着：那才是"壳层根本没参与"的证据。
  it('裸页（登录）不渲染外壳，notice 也不被裸页取走', async () => {
    const { w } = await mountApp('/login');
    useQuickCmdStore().notice = '不该出现';
    await flushPromises();
    expect(w.find('.toast').exists()).toBe(false);
    // 登录页没有外壳，谁都不该把 notice 当自己的东西清掉：清了就等于
    // "消息在没人看见的地方被消费掉了"。
    expect(useQuickCmdStore().notice).toBe('不该出现');
  });

  // 上一条留下来的东西必须有人接：watch 只在"值变了"的时候触发，
  // 裸页里攒下的那条如果只靠 watch，进外壳之后依然一声不响 ——
  // 于是"存着"变成"永远丢掉"，比直接清掉更难查。
  it('从裸页进外壳之后，攒下的 notice 补显示出来', async () => {
    const { w, router } = await mountApp('/login');
    useQuickCmdStore().notice = '攒着的话';
    await flushPromises();
    expect(w.find('.toast').exists()).toBe(false);

    await router.push('/quick');
    await flushPromises();
    expect(w.find('.toast').text()).toContain('攒着的话');
    expect(useQuickCmdStore().notice).toBe('');
  });
});
