<script setup lang="ts">
// 面板图标：内嵌 SVG，不依赖外网字体（单二进制离线可用）。
// 形状对齐 UI原型 使用的 Material Symbols 观感，统一 currentColor 描边。
withDefaults(defineProps<{ name: string; size?: number }>(), { size: 22 });

const PATHS: Record<string, string[]> = {
  bolt: ['M13 2 5 13h5l-1 9 8-11h-5z'],
  terminal: [
    'M3 5a2 2 0 0 1 2-2h14a2 2 0 0 1 2 2v14a2 2 0 0 1-2 2H5a2 2 0 0 1-2-2z',
    'M7 9l3 3-3 3',
    'M12 15h5',
  ],
  folder: ['M3 6a2 2 0 0 1 2-2h4l2 2h8a2 2 0 0 1 2 2v10a2 2 0 0 1-2 2H5a2 2 0 0 1-2-2z'],
  download: ['M12 3v11', 'M8 10l4 4 4-4', 'M4 17v2a2 2 0 0 0 2 2h12a2 2 0 0 0 2-2v-2'],
  settings: [
    'M12 9a3 3 0 1 0 0 6 3 3 0 0 0 0-6z',
    'M12 2v3',
    'M12 19v3',
    'M2 12h3',
    'M19 12h3',
    'M5 5l2 2',
    'M17 17l2 2',
    'M19 5l-2 2',
    'M7 17l-2 2',
  ],
};
</script>

<template>
  <svg
    class="ico"
    :width="size"
    :height="size"
    viewBox="0 0 24 24"
    fill="none"
    stroke="currentColor"
    stroke-width="1.8"
    stroke-linecap="round"
    stroke-linejoin="round"
    aria-hidden="true"
  >
    <path v-for="(d, i) in PATHS[name] ?? []" :key="i" :d="d" />
  </svg>
</template>

<style scoped>
.ico {
  display: block;
  flex-shrink: 0;
}
</style>
