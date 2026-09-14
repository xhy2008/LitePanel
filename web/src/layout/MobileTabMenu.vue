<script setup lang="ts">
import { computed, ref } from 'vue';
import { onClickOutside } from '../composables/useClickOutside';
import { useRoute, useRouter } from 'vue-router';
import AppIcon from '../components/AppIcon.vue';
import { TABS, tabOf } from '../tabs';

// 手机端左上角下拉：按钮图标 = 当前页图标，点开切换五个标签页（设计 16.1）。
const root = ref<HTMLElement | null>(null);
const open = ref(false);
const route = useRoute();
const router = useRouter();

const current = computed(() => tabOf(String(route.name ?? 'quick')));

function pick(name: string) {
  open.value = false;
  if (name !== route.name) router.push({ name });
}

onClickOutside(root, () => {
  open.value = false;
});
</script>

<template>
  <div ref="root" class="mobile-tabmenu">
    <button
      class="trigger"
      :class="{ on: open }"
      :title="current.label"
      :aria-label="current.label"
      :aria-expanded="open"
      @click="open = !open"
    >
      <AppIcon :key="current.name" :name="current.icon" :size="24" />
    </button>
    <div v-if="open" class="menu" role="menu">
      <button
        v-for="t in TABS"
        :key="t.name"
        class="item"
        :class="{ on: current.name === t.name }"
        role="menuitem"
        @click="pick(t.name)"
      >
        <AppIcon :name="t.icon" :size="18" />
        <span class="lbl">{{ t.label }}</span>
      </button>
    </div>
  </div>
</template>

<style scoped>
.mobile-tabmenu {
  position: relative;
}
.trigger {
  width: 40px;
  height: 40px;
  border: 0;
  border-radius: var(--radius);
  background: transparent;
  color: var(--text);
  display: grid;
  place-items: center;
  cursor: pointer;
}
.trigger.on {
  background: var(--bg);
  color: var(--accent);
}
.menu {
  position: absolute;
  top: calc(100% + 6px);
  left: 0;
  z-index: 40;
  min-width: 168px;
  background: var(--card);
  border: 1px solid var(--border);
  border-radius: var(--radius-md);
  padding: 6px;
  display: flex;
  flex-direction: column;
  gap: 2px;
}
.item {
  display: flex;
  align-items: center;
  gap: 10px;
  padding: 9px 10px;
  border: 0;
  border-radius: var(--radius);
  background: transparent;
  color: var(--text-dim);
  font-size: 13px;
  text-align: left;
  cursor: pointer;
}
.item.on {
  background: var(--card-2);
  color: var(--accent);
}
.lbl {
  line-height: 1;
}
</style>
