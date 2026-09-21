<script setup lang="ts">
import { computed, onMounted, onUnmounted, ref, watch } from 'vue';
import { useRoute } from 'vue-router';
import { useBreakpoint } from './composables/useBreakpoint';
import SideNav from './layout/SideNav.vue';
import MobileTabMenu from './layout/MobileTabMenu.vue';
import RightRail from './layout/RightRail.vue';
import MetricTicks from './components/metrics/MetricTicks.vue';
import MetricsPanel from './components/metrics/MetricsPanel.vue';
import { tickBars } from './composables/metricsTicks';
import ForcePassword from './views/ForcePassword.vue';
import { useAuthStore } from './stores/auth';
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
