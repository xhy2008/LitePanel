<script lang="ts">
export default { name: 'RightRail' };
</script>

<script setup lang="ts">
// 右栏指标容器：PC 常驻 128px；平板是可从右侧拉出的抽屉（默认收起）。
// 环形仪表与磁盘卡片在 M3/M2 填充，这里只提供容器与受控的开合状态。
withDefaults(
  defineProps<{
    mode?: 'fixed' | 'drawer';
    open?: boolean;
  }>(),
  { mode: 'fixed', open: false },
);
</script>

<template>
  <aside
    class="right-rail"
    :class="[mode, { open: mode === 'drawer' && open }]"
    aria-label="实时指标"
  >
    <div class="rail-h">
      <span>指标</span>
      <span class="rail-live"><i class="dot" />1s</span>
    </div>
    <slot />
  </aside>
</template>

<style scoped>
.right-rail {
  background: var(--bg-elev);
  border-left: 1px solid var(--border);
  padding: 10px 8px;
  overflow-y: auto;
}
.right-rail.drawer {
  position: fixed;
  top: 0;
  right: 0;
  bottom: 0;
  width: var(--rail-w);
  z-index: 30;
  transform: translateX(100%);
  transition: transform 0.2s ease;
}
.right-rail.drawer.open {
  transform: translateX(0);
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
</style>
