<script setup lang="ts">
import { computed } from 'vue';
import { useRoute, useRouter } from 'vue-router';
import AppIcon from '../components/AppIcon.vue';
import { TABS } from '../tabs';

// PC / 平板左栏：60px 纯图标，悬停出中文提示气泡（不显示文字标签）。
const route = useRoute();
const router = useRouter();
const active = computed(() => String(route.name ?? 'quick'));

function go(name: string) {
  router.push({ name });
}
</script>

<template>
  <nav class="side-nav" aria-label="主导航">
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
</template>

<style scoped>
.side-nav {
  background: var(--bg-elev);
  border-right: 1px solid var(--border);
  display: flex;
  flex-direction: column;
  align-items: center;
  padding-top: 10px;
  gap: 4px;
}
.tab {
  position: relative;
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
.tab:hover {
  background: var(--bg);
  color: var(--text);
}
.tab.on {
  color: var(--accent);
  background: var(--bg);
}
</style>
