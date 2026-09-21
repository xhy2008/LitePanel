<script lang="ts">
export default { name: 'MetricsPanel' };
</script>

<script setup lang="ts">
import { computed } from 'vue';
import Gauge from './Gauge.vue';
import DiskBars from './DiskBars.vue';
import type { Snapshot } from '../../api/metrics';

// 实时仪表面板：4 个环形仪表 + 磁盘条。
// 规格取自 UI原型-电脑版-v3 的 .ggrid/.gcell（128px 窄栏里纵向堆叠），
// 右栏与登录页共用同一份 —— 两处各写一遍 GPU 的 unavailable/reason
// 组合必然漂移，所以只留一处。
const props = defineProps<{ snapshot: Snapshot | null }>();

const s = computed(() => props.snapshot);

const gpu = computed(() => s.value?.gpu ?? null);
const vram = computed(() => s.value?.vram ?? null);

// GPU 与显存共用一个探测结果：NVML 不可用时两者的原因相同。
const gpuReason = computed(() => gpu.value?.reason || vram.value?.reason || '');

function num(v: number | null | undefined): number | undefined {
  return v === null || v === undefined ? undefined : v;
}

// 环外次要读数：只放「一眼想知道的第二件事」，各一行。
const cpuSub = computed(() =>
  s.value?.cpu ? `负载 ${s.value.cpu.load1.toFixed(2)}` : '',
);
const memSub = computed(() => {
  const m = s.value?.mem;
  return m ? `${(m.used / 1e9).toFixed(1)} / ${(m.total / 1e9).toFixed(1)}G` : '';
});
// GPU 副读数：“67°C 142W”。温度和功耗属 M3（NVML）范围，现在后端不返回，
// 缺席时就不写，而不是渲染成“undefined°C undefinedW”。
const gpuSub = computed(() => {
  const g = gpu.value;
  if (!g?.available || g.temp_c === undefined || g.power_w === undefined) return '';
  return `${Math.round(g.temp_c)}°C ${Math.round(g.power_w)}W`;
});
// 显存与利用率是同一个 GPUStat，所以取 percent/used/total，不是 vram_xxx。
const vramSub = computed(() => {
  const v = vram.value;
  if (!v?.available || v.total === undefined) return '';
  return `${((v.used ?? 0) / 1e9).toFixed(1)} / ${(v.total / 1e9).toFixed(1)}G`;
});
</script>

<template>
  <div class="metrics-panel">
    <div class="ggrid">
      <div class="gcell">
        <Gauge name="CPU" :value="num(s?.cpu?.percent)" :sub="cpuSub" color="#FF6600" />
      </div>
      <div class="gcell">
        <Gauge name="内存" :value="num(s?.mem?.percent)" :sub="memSub" color="#3ba7ff" />
      </div>
      <div class="gcell">
        <Gauge
          name="GPU"
          :value="num(gpu?.available ? gpu?.percent : null)"
          :unavailable="!gpu?.available"
          :reason="gpuReason"
          :sub="gpuSub"
          color="#2ecc71"
        />
      </div>
      <div class="gcell">
        <Gauge
          name="显存"
          :value="num(vram?.available ? vram?.percent : null)"
          :unavailable="!vram?.available"
          :reason="gpuReason"
          :sub="vramSub"
          color="#f5a623"
        />
      </div>
    </div>
    <div class="rail-h disk-head"><span>磁盘</span></div>
    <DiskBars :disks="s?.disks ?? []" />
  </div>
</template>

<style scoped>
/* 原型 .ggrid：纵向堆叠，间距 7px */
.ggrid {
  display: flex;
  flex-direction: column;
  gap: 7px;
}
/* 原型 .gcell：卡片底 + 居中 */
.gcell {
  background: var(--card);
  border: 1px solid var(--border-soft);
  border-radius: var(--radius-md);
  padding: 9px 4px 7px;
  display: flex;
  flex-direction: column;
  align-items: center;
}
.disk-head {
  margin: 8px 0 6px;
}
.rail-h {
  font-size: 9px;
  color: var(--text-mute);
  letter-spacing: 1px;
  text-transform: uppercase;
}
</style>