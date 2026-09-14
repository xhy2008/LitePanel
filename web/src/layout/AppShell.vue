<script setup lang="ts">
import { computed } from 'vue';
import { useRoute, useRouter } from 'vue-router';
import { useBreakpoint } from '../composables/useBreakpoint';
import MetricTicks from '../components/metrics/MetricTicks.vue';
import AppIcon from '../components/AppIcon.vue';

// 5 个一级 tab；图标名沿用 UI原型 的 Material Symbols 字形。
const TABS = [
  { name: 'quick', label: '快捷命令', icon: 'bolt' },
  { name: 'term', label: '终端', icon: 'terminal' },
  { name: 'files', label: '文件管理', icon: 'folder' },
  { name: 'downloads', label: '下载', icon: 'download' },
  { name: 'settings', label: '设置', icon: 'settings' },
];

const route = useRoute();
const router = useRouter();
const { bp } = useBreakpoint();

const active = computed(() => String(route.name ?? 'quick'));

function go(name: string) {
  if (name !== active.value) router.push({ name });
}
</script>

<template>
  <div class="shell" :class="'bp-' + bp">
    <header v-if="bp === 'phone'" class="phone-statusbar">
      <MetricTicks />
    </header>

    <nav v-if="bp !== 'phone'" class="icon-rail" aria-label="主导航">
      <button
        v-for="t in TABS"
        :key="t.name"
        class="tab"
        :class="{ on: active === t.name }"
        :title="t.label"
        :aria-label="t.label"
        @click="go(t.name)"
      >
        <AppIcon :name="t.icon" />
      </button>
    </nav>

    <main class="content">
      <router-view />
    </main>

    <aside v-if="bp === 'pc'" class="metrics-rail" aria-label="实时指标">
      <div class="rail-h">
        <span>指标</span>
        <span class="rail-live"><i class="dot" />1s</span>
      </div>
    </aside>

    <nav v-if="bp === 'phone'" class="phone-tabbar" aria-label="主导航">
      <button
        v-for="t in TABS"
        :key="t.name"
        class="tab"
        :class="{ on: active === t.name }"
        @click="go(t.name)"
      >
        <AppIcon :name="t.icon" :size="20" />
        <span class="lbl">{{ t.label }}</span>
      </button>
    </nav>
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
  grid-template-rows: var(--topbar-h) 1fr var(--tabbar-h);
  grid-template-areas: 'bar' 'main' 'tabbar';
}

.icon-rail {
  grid-area: nav;
  background: var(--bg-elev);
  border-right: 1px solid var(--border);
  display: flex;
  flex-direction: column;
  align-items: center;
  padding-top: 10px;
  gap: 4px;
}
.icon-rail .tab {
  width: 44px;
  height: 44px;
  border: 0;
  border-radius: var(--radius);
  background: transparent;
  color: var(--text-dim);
  cursor: pointer;
  display: grid;
  place-items: center;
}
.icon-rail .tab:hover {
  background: var(--bg);
  color: var(--text);
}
.icon-rail .tab.on {
  color: var(--accent);
  background: var(--bg);
}

.content {
  grid-area: main;
  overflow: auto;
}

.metrics-rail {
  grid-area: rail;
  background: var(--bg-elev);
  border-left: 1px solid var(--border);
  padding: 10px 8px;
}
.rail-h {
  display: flex;
  justify-content: space-between;
  align-items: center;
  font-size: 11px;
  color: var(--text-dim);
  margin-bottom: 8px;
}
.rail-live {
  display: flex;
  align-items: center;
  gap: 4px;
  color: var(--ok);
}
.rail-live .dot {
  width: 6px;
  height: 6px;
  border-radius: 50%;
  background: var(--ok);
}

.phone-statusbar {
  grid-area: bar;
  background: var(--bg-elev);
  border-bottom: 1px solid var(--border);
  display: flex;
  align-items: center;
  justify-content: flex-end;
  padding: 0 12px;
}
.phone-tabbar {
  grid-area: tabbar;
  background: var(--bg-elev);
  border-top: 1px solid var(--border);
  display: grid;
  grid-template-columns: repeat(5, 1fr);
}
.phone-tabbar .tab {
  border: 0;
  background: transparent;
  color: var(--text-dim);
  display: flex;
  flex-direction: column;
  align-items: center;
  justify-content: center;
  gap: 2px;
  font-size: 10px;
  cursor: pointer;
}
.phone-tabbar .tab.on {
  color: var(--accent);
}
.phone-tabbar .lbl {
  line-height: 1;
}
</style>
