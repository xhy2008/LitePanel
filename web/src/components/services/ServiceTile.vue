<script setup lang="ts">
import { computed } from 'vue';
import AppIcon from '../AppIcon.vue';
import { describeService } from '../../stores/services';
import type { ServiceRow } from '../../api/services';

// 单个服务磁贴。样式对齐 UI原型 的 .tile / .st / .kb / .sw / .ib。
const props = defineProps<{ row: ServiceRow; busy?: boolean }>();
// edit 由点击信息区触发（打开编辑表单）。磁贴本身不整体可点：
// 磁贴上只有开关和日志两个手感的控件，把整块做成按钮会让开关
// 每次都在按钮里面，移动端误触率很高。
const emit = defineEmits<{
  toggle: [id: number];
  log: [id: number];
  edit: [id: number];
}>();

// 文案的唯一来源是 store 里的 describeService（D21 三态）。
// 视图自己拼字符串的话，"异常退出"的写法会在两处漂移，
// 而排查故障时读到的就是完全错误的含义。
const desc = computed(() => describeService(props.row));
const on = computed(
  () => props.row.state === 'running' || props.row.state === 'starting',
);
const toneClass = computed(() => 'st-' + desc.value.tone);

function clickSwitch() {
  // busy 时吞掉点击：连点不是 UI 瑕疵，是真的会把服务反复启停。
  if (props.busy) return;
  emit('toggle', props.row.id);
}
</script>

<template>
  <div class="tile">
    <div class="ti" :class="'ti-' + desc.tone">
      <AppIcon :name="desc.icon" :size="18" />
    </div>
    <div class="tinfo" role="button" @click="emit('edit', row.id)">
      <div class="tname">
        {{ row.name }}
        <span class="kb" :class="row.kind === 'systemd' ? 'kb-s' : 'kb-c'">
          {{ desc.badge }}
        </span>
      </div>
      <div class="tsub">{{ row.kind === 'systemd' ? row.unit : row.start_cmd }}</div>
      <div class="st" :class="toneClass">
        <AppIcon :name="desc.icon" :size="11" />
        <span>{{ desc.text }}</span>
      </div>
    </div>
    <button class="ib" title="查看输出" @click="emit('log', row.id)">
      <AppIcon name="receipt" :size="17" />
    </button>
    <div
      class="sw"
      :class="{ on, busy }"
      role="switch"
      :aria-checked="on"
      :aria-disabled="busy ? 'true' : 'false'"
      @click="clickSwitch"
    />
  </div>
</template>

<style scoped>
.tile {
  background: var(--card);
  border: 1px solid var(--border-soft);
  border-radius: var(--radius-md);
  padding: 10px 12px;
  display: flex;
  align-items: center;
  gap: 9px;
  margin-bottom: 7px;
  min-height: 56px;
}
.tile:active {
  background: var(--card-2);
}
.ti {
  width: 34px;
  height: 34px;
  border-radius: 9px;
  flex-shrink: 0;
  display: flex;
  align-items: center;
  justify-content: center;
}
.ti-run {
  background: rgba(46, 204, 113, 0.14);
  color: var(--ok);
}
.ti-good {
  background: rgba(149, 162, 180, 0.12);
  color: var(--text-dim);
}
.ti-bad {
  background: rgba(255, 77, 79, 0.13);
  color: var(--err);
}
.ti-idle {
  background: rgba(149, 162, 180, 0.12);
  color: var(--text-mute);
}
.tinfo {
  flex: 1;
  min-width: 0;
}
.tname {
  font-size: 13px;
  font-weight: 600;
  display: flex;
  align-items: center;
  gap: 5px;
}
.tsub {
  font-size: 10px;
  color: var(--text-mute);
  margin-top: 2px;
  white-space: nowrap;
  overflow: hidden;
  text-overflow: ellipsis;
  font-family: ui-monospace, Menlo, Consolas, monospace;
}
.st {
  font-size: 10px;
  display: flex;
  align-items: center;
  gap: 4px;
  margin-top: 3px;
  font-variant-numeric: tabular-nums;
}
.st-run {
  color: var(--ok);
}
.st-bad {
  color: var(--err);
}
.st-good,
.st-idle {
  color: var(--text-dim);
}
.kb {
  font-size: 8px;
  padding: 1px 4px;
  border-radius: 3px;
  font-weight: 700;
  flex-shrink: 0;
}
.kb-c {
  background: rgba(59, 167, 255, 0.16);
  color: var(--info);
}
.kb-s {
  background: rgba(155, 89, 182, 0.18);
  color: #b57edc;
}
.sw {
  width: 42px;
  height: 24px;
  border-radius: 12px;
  background: var(--border);
  position: relative;
  cursor: pointer;
  transition: background 0.2s;
  flex-shrink: 0;
}
.sw::after {
  content: '';
  position: absolute;
  top: 2px;
  left: 2px;
  width: 20px;
  height: 20px;
  border-radius: 50%;
  background: #fff;
  transition: transform 0.2s;
}
.sw.on {
  background: var(--ok);
}
.sw.on::after {
  transform: translateX(18px);
}
/* 请求在飞：开关不动但给出正在处理的暗示，别让人以为点没响应。 */
.sw.busy {
  cursor: progress;
  opacity: 0.55;
}
.ib {
  width: 32px;
  height: 32px;
  border-radius: 8px;
  background: var(--card-2);
  border: none;
  color: var(--text-mute);
  display: flex;
  align-items: center;
  justify-content: center;
  cursor: pointer;
  flex-shrink: 0;
}
</style>
