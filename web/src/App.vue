<script setup lang="ts">
import { computed, onMounted, onUnmounted, ref, watch } from 'vue';
import { useRoute } from 'vue-router';
import { useBreakpoint } from './composables/useBreakpoint';
import SideNav from './layout/SideNav.vue';
import MobileTabMenu from './layout/MobileTabMenu.vue';
import RightRail from './layout/RightRail.vue';
import MetricTicks from './components/metrics/MetricTicks.vue';
import Gauge from './components/metrics/Gauge.vue';
import DiskBars from './components/metrics/DiskBars.vue';
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

// MetricTicks 手机顶栏的竖条（固定 6 格）：4 指标 + 分隔 + 使用率最高的盘。
const tickBars = computed(() => {
  const s = metricsState.value.snapshot;
  const bars: { name: string; value?: number; color?: string }[] = [
    { name: 'CPU', value: s?.cpu?.percent ?? undefined, color: '#FF6600' },
    { name: '内存', value: s?.mem?.percent ?? undefined, color: '#3ba7ff' },
    { name: 'GPU', value: s?.gpu?.available ? s.gpu.percent : undefined, color: '#2ecc71' },
    { name: '显存', value: s?.vram?.available ? s.vram.vram_percent : undefined, color: '#f5a623' },
    { name: '/', value: undefined }, // separator
  ];
  const disks = s?.disks ?? [];
  const topDisk = disks.length > 0
    ? disks.reduce((a, b) => (a.percent >= b.percent ? a : b))
    : null;
  bars.push({
    name: topDisk?.mountpoint ?? '--',
    value: topDisk?.percent ?? undefined,
    color: topDisk && topDisk.percent > 90 ? 'var(--err)' : '#f5a623',
  });
  return bars;
});

// 右栏 4 个 Gauge 的次要读数。
function cpuSub(s: typeof metricsState.value.snapshot): string {
  if (!s?.cpu) return '';
  return `负载 ${s.cpu.load1.toFixed(2)}`;
}
function memSub(s: typeof metricsState.value.snapshot): string {
  if (!s?.mem) return '';
  const t = (s.mem.total / 1e9).toFixed(1);
  const u = (s.mem.used / 1e9).toFixed(1);
  return `${u} / ${t}G`;
}
function gpuSub(s: typeof metricsState.value.snapshot): string {
  if (!s?.gpu?.available) return '';
  return `${s.gpu.temp_c}°C ${s.gpu.power_w}W`;
}
function vramSub(s: typeof metricsState.value.snapshot): string {
  if (!s?.vram?.available) return '';
  const t = (s.vram.vram_total / 1e9).toFixed(1);
  const u = (s.vram.vram_used / 1e9).toFixed(1);
  return `${u} / ${t}G`;
}

// 平板：右栏是抽屉，默认收起。
const railOpen = ref(false);
watch(bp, () => (railOpen.value = false));
</script>

<template>
  <router-view v-if="bare" />

  <div v-else class="shell" :class="'bp-' + bp">
    <header v-if="bp === 'phone'" class="mobile-topbar">
      <MobileTabMenu />
      <MetricTicks :bars="tickBars" />
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
      <div class="r-gauges">
        <Gauge name="CPU" :value="metricsState.value.snapshot?.cpu?.percent ?? undefined" :sub="cpuSub(metricsState.value.snapshot)" color="#FF6600" />
        <Gauge name="内存" :value="metricsState.value.snapshot?.mem?.percent ?? undefined" :sub="memSub(metricsState.value.snapshot)" color="#3ba7ff" />
        <Gauge name="GPU" :value="metricsState.value.snapshot?.gpu?.available ? metricsState.value.snapshot.gpu.percent : undefined" :unavailable="!metricsState.value.snapshot?.gpu?.available" :reason="!metricsState.value.snapshot?.gpu?.available ? metricsState.value.snapshot?.gpu?.reason : undefined" :sub="gpuSub(metricsState.value.snapshot)" color="#2ecc71" />
        <Gauge name="显存" :value="metricsState.value.snapshot?.vram?.available ? metricsState.value.snapshot.vram.vram_percent : undefined" :unavailable="!metricsState.value.snapshot?.vram?.available" :reason="!metricsState.value.snapshot?.vram?.available && metricsState.value.snapshot?.gpu?.reason ? metricsState.value.snapshot.gpu.reason : undefined" :sub="vramSub(metricsState.value.snapshot)" color="#f5a623" />
      </div>
      <div class="r-disks">
        <div class="rail-h" style="margin-top:8px">磁盘</div>
        <DiskBars :disks="metricsState.value.snapshot?.disks ?? []" />
      </div>
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
