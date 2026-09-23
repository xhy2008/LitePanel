<script setup lang="ts">
// 面板图标：内嵌 SVG，不依赖外网字体（单二进制离线可用）。
// 形状对齐 UI原型 使用的 Material Symbols 观感，统一 currentColor 描边。
withDefaults(defineProps<{ name: string; size?: number }>(), { size: 22 });
import { PATHS } from './iconPaths';

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
