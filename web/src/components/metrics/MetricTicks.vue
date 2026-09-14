<script lang="ts">
export interface TickBar {
  name: string;
  value?: number; // 0..100，undefined = 无数据
  color?: string;
}
</script>

<script setup lang="ts">
import { computed } from 'vue';
// 顶部 6 根指标竖条：CPU / 内存 / GPU / 显存 /（分隔）/ 盘1 / 盘2。
// 对齐 UI原型-手机版-v2 的 .vbs：值 + 按比例填充的轨道 + 名称。
const props = withDefaults(defineProps<{ bars?: TickBar[] }>(), { bars: () => [] });

const DEFAULT_SLOTS = ['CPU', '内存', 'GPU', '显存', '/', 'hdd'];

function pct(v: number | undefined): number {
  if (v === undefined || Number.isNaN(v)) return 0;
  return Math.max(0, Math.min(100, Math.round(v)));
}

function colorOf(b: TickBar): string {
  if (b.color) return b.color;
  const v = pct(b.value);
  if (v >= 85) return 'var(--err)';
  if (v >= 60) return 'var(--warn)';
  return 'var(--ok)';
}

const slots = computed<TickBar[]>(() =>
  Array.from({ length: 6 }, (_, i) => props.bars[i] ?? { name: DEFAULT_SLOTS[i] }),
);
</script>

<template>
  <div class="vbs" role="img" aria-label="指标概览">
    <template v-for="(b, i) in slots" :key="i">
      <span v-if="i === 4" class="vb-sep" />
      <div class="tick">
        <div class="tick-v">{{ b.value === undefined ? '--' : pct(b.value) + '%' }}</div>
        <div class="tick-track">
          <div class="tick-fill" :style="{ height: pct(b.value) + '%', background: colorOf(b) }" />
        </div>
        <div class="tick-n">{{ b.name }}</div>
      </div>
    </template>
  </div>
</template>

<style scoped>
.vbs { display: flex; align-items: flex-end; gap: 6px; }
.vb-sep { width: 1px; height: 20px; background: var(--border); margin: 0 2px; }
.tick { display: flex; flex-direction: column; align-items: center; gap: 2px; width: 26px; }
.tick-v { font-size: 9px; color: var(--text-dim); line-height: 1; }
.tick-track {
  width: 6px; height: 22px; border-radius: 3px; background: var(--border);
  display: flex; align-items: flex-end; overflow: hidden;
}
.tick-fill { width: 100%; border-radius: 3px; transition: height 0.4s ease; }
.tick-n {
  font-size: 9px; color: var(--text-dim); line-height: 1; max-width: 26px;
  overflow: hidden; white-space: nowrap;
}
</style>
