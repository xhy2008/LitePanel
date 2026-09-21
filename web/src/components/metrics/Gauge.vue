<script lang="ts">
export default { name: 'Gauge' };
</script>

<script setup lang="ts">
import { computed } from 'vue';

const props = withDefaults(
  defineProps<{
    name: string;
    value?: number | null;
    sub?: string;
    unavailable?: boolean;
    reason?: string;
    color?: string;
  }>(),
  { value: undefined, sub: '', unavailable: false, reason: '', color: '#FF6600' },
);

// r=33，环径 72px，描边 6px（v3 原型）
const CIR = 2 * Math.PI * 33; // ≈207.345

const safePct = computed(() => {
  if (props.value === undefined || props.value === null) return null;
  return Math.max(0, Math.min(100, Math.round(props.value)));
});

const dashArray = computed(() => {
  const p = safePct.value;
  if (p === null) return `${CIR.toFixed(1)} ${CIR.toFixed(1)}`; // 空环
  const fill = (CIR * p) / 100;
  return `${fill.toFixed(1)} ${CIR.toFixed(1)}`;
});

const displayValue = computed(() => {
  if (props.unavailable || safePct.value === null) return '--';
  return String(safePct.value);
});
</script>

<template>
  <div class="g" :class="{ unavailable }">
    <svg width="72" height="72" viewBox="0 0 72 72">
      <circle class="g-bg" cx="36" cy="36" r="33" />
      <circle
        class="g-fg"
        cx="36"
        cy="36"
        r="33"
        :stroke="unavailable ? 'var(--border)' : color"
        :stroke-dasharray="dashArray"
        transform="rotate(-90 36 36)"
      />
    </svg>
    <div class="g-c">
      <div class="g-v">
        {{ displayValue }}<small v-if="!unavailable && safePct !== null">%</small>
      </div>
      <div class="g-n">{{ name }}</div>
      <div class="g-ua" v-if="unavailable">不可用</div>
      <div v-if="unavailable && reason" class="g-reason">{{ reason }}</div>
    </div>
    <div v-if="sub && !unavailable" class="g-x">{{ sub }}</div>
  </div>
</template>

<style scoped>
.g {
  position: relative;
  display: flex;
  flex-direction: column;
  align-items: center;
  width: 72px;
}
.g.unavailable {
  opacity: 0.5;
}
.g-bg {
  fill: none;
  stroke: var(--border);
  stroke-width: 6;
}
.g-fg {
  fill: none;
  stroke-width: 6;
  stroke-linecap: round;
  transition: stroke-dasharray 0.8s cubic-bezier(0.4, 0, 0.2, 1);
}
.g-c {
  position: absolute;
  top: 0;
  left: 0;
  width: 72px;
  height: 72px;
  display: flex;
  flex-direction: column;
  align-items: center;
  justify-content: center;
  pointer-events: none;
}
.g-v {
  font-size: 17px;
  font-weight: 700;
  color: var(--text);
  line-height: 1.2;
}
.g-v small {
  font-size: 10px;
  font-weight: 400;
}
.g-n {
  font-size: 10px;
  color: var(--text-dim);
  line-height: 1.3;
}
.g-reason {
  font-size: 7px;
  color: var(--text-dim);
  max-width: 68px;
  text-align: center;
  line-height: 1.2;
}
.g-x {
  font-size: 8.5px;
  color: var(--text-dim);
  line-height: 1.3;
  margin-top: 2px;
}
</style>