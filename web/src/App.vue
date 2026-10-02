<script setup lang="ts">
import { computed, onMounted, onUnmounted, ref, watch } from 'vue';
import { useRoute } from 'vue-router';
import { useBreakpoint } from './composables/useBreakpoint';
import SideNav from './layout/SideNav.vue';
import MobileTabMenu from './layout/MobileTabMenu.vue';
import JobDrawer from './components/jobs/JobDrawer.vue';
import RightRail from './layout/RightRail.vue';
import MetricTicks from './components/metrics/MetricTicks.vue';
import MetricsPanel from './components/metrics/MetricsPanel.vue';
import { tickBars } from './composables/metricsTicks';
import ForcePassword from './views/ForcePassword.vue';
import { useAuthStore } from './stores/auth';
import { useQuickCmdStore } from './stores/quickcmd';
import { useTerminalStore } from './stores/terminal';
import { attachMetrics, metricsState } from './composables/useMetrics';
import { ws } from './api/ws';
import { getApi } from './api/inject';
import type { DiskUsage } from './api/metrics';

// 三栏 Grid 骨架 + 断点分支（设计 16.1）。
// 用组件级 v-if 而不是 CSS display:none：手机端不该为看不见的右栏
// 付出渲染与 WS 订阅成本。
const route = useRoute();
const { bp } = useBreakpoint();
const auth = useAuthStore();

// 强制改密闸门：后端在未改密时对业务 API 返回 403，
// 前端直接把内容区换成改密表单，避免用户对着点不动的按钮猜。
const gated = computed(() => auth.loggedIn && auth.mustChangePassword);

// 登录等裸页不套外壳。
const bare = computed(() => route.matched.some((r) => r.meta?.bare));

// 仪表盘数据接线（useMetrics）。
// 接入点在新业务 API 前：首屏不能等 WS 握手，用 HTTP 快照垫第一帧。
let stopMetrics: (() => void) | null = null;
onMounted(() => {
  let apiDeps: ReturnType<typeof getApi> | null = null;
  try {
    apiDeps = getApi();
  } catch {
    return;
  }
  let wsClient: ReturnType<typeof ws>;
  try {
    wsClient = ws();
  } catch {
    return;
  }
  const { api } = apiDeps;
  attachMetrics({
    fetchSnap: () =>
      api.get<import('./api/metrics').Snapshot>('/api/metrics/snapshot'),
    wsClient,
  }).then((s) => {
    stopMetrics = s;
  });
});
onUnmounted(() => {
  stopMetrics?.();
  stopMetrics = null;
});

// MetricTicks 手机顶栏的竖条（固定 6 格）：映射与登录页共用一份。
const bars = computed(() => tickBars(metricsState.value.snapshot));

// 一次性提示（toast）。放在壳层而不是任何一个页面里，理由不是顺手：
// 点快捷命令会跳终端页，那个页面在跳转中被卸载 —— 谁在页面里显示 notice，
// notice 就跟着谁一起没了，而"已新建会话 lp-4 执行"这句话最需要被看见的
// 时机恰好就是跳过去之后。同理"会话已退出：跑任务"是后台轮询发现的，
// 后台轮询没有返回值可读。
//
// 显示后必须把 notice 取走（clearNotice）：notice 的值不变就不会再触发
// watch，于是"又新建了一个会话"这类每次都一样的文案，第二次点就悄无声息
// —— 而用户正因为看不见才连点。
const quickcmd = useQuickCmdStore();
const terminal = useTerminalStore();
const toastMsg = ref('');
let toastTimer: ReturnType<typeof setTimeout> | null = null;

function showToast(msg: string) {
  toastMsg.value = msg;
  // 每条提示各自重新计时：连着两条时按最后一条算，否则第一条的定时器
  // 会把刚出现的第二条抹掉。
  if (toastTimer) clearTimeout(toastTimer);
  // 必须自己消失：常驻的提示会被读成"当前状态"，而它说的其实是
  // "刚刚发生过一件事"。
  toastTimer = setTimeout(() => (toastMsg.value = ''), 2600);
}

function drain(msg: string, clear: () => void) {
  if (!msg) return;
  // 裸页（登录）不渲染外壳，也就没有 toast —— 这时候绝对不能清空：
  // 清掉就等于"消息在一个没人能看见的地方被消费掉了"，用户既看不到
  // 也再不会看到。留着它，登录进外壳之后自然会被取走。
  if (bare.value) return;
  showToast(msg);
  clear();
}

watch(() => quickcmd.notice, (m) => drain(m, () => quickcmd.clearNotice()));
watch(() => terminal.notice, (m) => drain(m, () => terminal.clearNotice()));

// 补一次账：上面"裸页不许清空"把消息留了下来，而 watch 只在值变化的
// 时候触发 —— 登录页里躺着的那条，进外壳时值并没有变，谁也不会再提。
// 没有这一步，"留着"就变成"永远丢掉"，比清掉更难查。
//
// 触发点是 bare 而不是 onMounted：App 自己不参与路由，从登录页进外壳
// 时它不会重新挂载，onMounted 一辈子只跑一次，正好错过要补的那一次。
function drainPending() {
  drain(quickcmd.notice, () => quickcmd.clearNotice());
  drain(terminal.notice, () => terminal.clearNotice());
}

// 不需要 onMounted 版：进入非裸页只有"从裸页过来"这一条路（路由守卫
// 会把未登录访问业务页的先赶到登录页），而那条路 watch(bare) 必然触发。
// 变异测试删掉 onMounted(drainPending) 全绿 —— 它就是死代码，删。
watch(bare, (isBare) => {
  if (!isBare) drainPending();
});

onUnmounted(() => {
  if (toastTimer) clearTimeout(toastTimer);
  toastTimer = null;
});

// 平板：右栏是抽屉，默认收起。
const railOpen = ref(false);
watch(bp, () => (railOpen.value = false));
</script>

<template>
  <router-view v-if="bare" />

  <div v-else class="shell" :class="'bp-' + bp">
    <header v-if="bp === 'phone'" class="mobile-topbar">
      <MobileTabMenu />
      <MetricTicks :bars="bars" />
    </header>

    <SideNav v-if="bp !== 'phone'" class="nav-cell" />

    <nav
      v-if="bp === 'tablet'"
      class="rail-drawer-toggle"
      :class="{ open: railOpen }"
      aria-label="切换指标栏"
      @click="railOpen = !railOpen"
    >
      <span class="grip" />
    </nav>

    <main class="content">
      <ForcePassword v-if="gated" />
      <router-view v-else />
    </main>

    <div v-if="toastMsg" class="toast" role="status">{{ toastMsg }}</div>

    <!-- 全局任务抽屉。放在壳层而不是文件页里是刻意的：任务的生命周期与
         "用户当前看哪个页"无关，抽屉跟着文件页卸载就等于把后端"关浏览器
         也不中断"的能力在界面上抹掉 —— 看不见就会被当成没生效，用户会
         去重做一遍，而那正是任务队列要防的重复劳动。 -->
    <JobDrawer />

    <RightRail
      v-if="bp !== 'phone'"
      class="rail-cell"
      :mode="bp === 'pc' ? 'fixed' : 'drawer'"
      :open="railOpen"
    >
      <MetricsPanel :snapshot="metricsState.value.snapshot" />
    </RightRail>
  </div>
</template>

<style scoped>
.toast {
  position: fixed;
  top: 14px;
  left: 50%;
  transform: translateX(-50%);
  z-index: 9999;
  background: var(--card);
  border: 1px solid var(--border);
  border-left: 3px solid var(--accent);
  border-radius: var(--radius);
  padding: 8px 14px;
  font-size: 12px;
  color: var(--text);
  box-shadow: var(--shadow);
}

.shell {
  height: 100%;
  display: grid;
  grid-template-columns: var(--nav-w) 1fr;
  grid-template-rows: 1fr;
  grid-template-areas: 'nav main';
}
.shell.bp-pc {
  grid-template-columns: var(--nav-w) 1fr var(--rail-w);
  grid-template-areas: 'nav main rail';
}
.shell.bp-phone {
  grid-template-columns: 1fr;
  grid-template-rows: var(--topbar-h) 1fr;
  grid-template-areas: 'bar' 'main';
}

.nav-cell {
  grid-area: nav;
}
.rail-cell {
  grid-area: rail;
}
.content {
  grid-area: main;
  overflow: auto;
  position: relative;
}

.mobile-topbar {
  grid-area: bar;
  height: var(--topbar-h);
  background: var(--bg-elev);
  border-bottom: 1px solid var(--border);
  display: flex;
  align-items: center;
  gap: 8px;
  padding: 0 10px;
}

.rail-drawer-toggle {
  position: fixed;
  top: 50%;
  right: 0;
  transform: translateY(-50%);
  z-index: 31;
  width: 18px;
  height: 56px;
  background: var(--bg-elev);
  border: 1px solid var(--border);
  border-right: 0;
  border-radius: var(--radius) 0 0 var(--radius);
  display: grid;
  place-items: center;
  cursor: pointer;
}
.rail-drawer-toggle.open {
  right: var(--rail-w);
}
.grip {
  width: 3px;
  height: 22px;
  border-radius: 2px;
  background: var(--text-mute);
}
</style>
