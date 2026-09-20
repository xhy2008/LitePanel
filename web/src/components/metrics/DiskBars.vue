<script lang="ts">
export default { name: 'DiskBars' };
</script>

<script setup lang="ts">
import type { DiskUsage } from '../../api/metrics';

defineProps<{ disks: DiskUsage[] }>();

function fmtByte(v: number): string {
  if (v >= 1e9) return (v / 1e9).toFixed(1) + 'G';
  if (v >= 1e6) return (v / 1e6).toFixed(1) + 'M';
  return (v / 1e3) + 'K';
}
</script>

<template>
  <div class="disk-bars">
    <div v-for="d in disks" :key="d.mountpoint" class="disk-item">
      <div class="disk-h">
        <span class="disk-name trunc">{{ d.mountpoint }}</span>
        <span class="disk-pct">{{ d.percent.toFixed(1) }}%</span>
      </div>
      <div class="disk-track">
        <div
          class="disk-fill"
          :class="{ 'disk-fill--warn': d.percent > 90 }"
          :style="{ width: Math.min(d.percent, 100) + '%' }"
        />
      </div>
      <div class="disk-size">{{ fmtByte(d.used) }} / {{ fmtByte(d.total) }}</div>
    </div>
  </div>
</template>

<style scoped>
.disk-bars {
  display: flex;
  flex-direction: column;
  gap: 8px;
}
.disk-item {
  display: flex;
  flex-direction: column;
  gap: 1px;
}
.disk-h {
  display: flex;
  justify-content: space-between;
  align-items: baseline;
  font-size: 10px;
}
.disk-name {
  color: var(--text);
  max-width: 60px;
}
.disk-name.trunc {
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
}
.disk-pct {
  color: var(--text-dim);
}
.disk-track {
  height: 4px;
  border-radius: 2px;
  background: var(--border);
  overflow: hidden;
}
.disk-fill {
  height: 100%;
  border-radius: 2px;
  background: var(--ok);
  transition: width 0.4s ease;
}
.disk-fill--warn {
  background: var(--err);
}
.disk-size {
  font-size: 8.5px;
  color: var(--text-dim);
}
</style>