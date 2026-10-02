<script setup lang="ts">
import { computed } from 'vue';
import AppIcon from '../AppIcon.vue';

// 右键菜单（设计 8.5）。PC 右键与手机长按共用同一个组件：菜单项、禁用
// 逻辑、危险项配色都只有一份，两处分别实现迟早会漂移成"手机上少几个功能"。
export interface MenuItem {
  key: string;
  label: string;
  icon?: string;
  /** 无剪贴板时的粘贴之类：灰掉且不可点。 */
  disabled?: boolean;
  /** 删除这类不可逆操作：红色，并在其后加一条分隔感。 */
  danger?: boolean;
  /** 在这一项之前画一条分隔线，用来分组（打开 |剪贴板| 删除）。 */
  sep?: boolean;
}

const props = defineProps<{ items: MenuItem[]; x: number; y: number }>();
const emit = defineEmits<{ pick: [key: string]; close: [] }>();

// 菜单尺寸估定值：真实宽度要渲染后才知道，而这里要在渲染前算出位置。
// 用估算值换"不闪一下"（先画在超界位置再跳回来会闪）。手机端菜单固定
// 窄，估算偏差不影响"是否出界"的判断。
const EST_W = 190;
const EST_H = 40;

const style = computed(() => {
  const vw = globalThis.innerWidth || 360;
  const vh = globalThis.innerHeight || 640;
  let { x, y } = props;
  if (x + EST_W > vw) x = Math.max(4, vw - EST_W - 4);
  if (y + EST_H * Math.max(props.items.length, 1) > vh) {
    y = Math.max(4, vh - EST_H * Math.max(props.items.length, 1) - 4);
  }
  return { left: `${x}px`, top: `${y}px` };
});

function onPick(it: MenuItem) {
  if (it.disabled) return;
  emit('pick', it.key);
  emit('close');
}

// 长按/右键会在触屏与鼠标上带出浏览器原生菜单、并把下面的文字拖成选中
// 态。菜单容器必须自己吞掉这两个默认行为，否则我们的菜单会叠在系统菜单
// 下面，或一长按就选中整行文件名。
function block(e: Event) {
  e.preventDefault();
}
</script>

<template>
  <div class="scrim" @click="emit('close')" @contextmenu.prevent="block" />
  <div
    v-if="items.length"
    class="menu"
    role="menu"
    :style="style"
    @contextmenu.prevent="block"
    @selectstart="block"
    @click.stop
  >
    <div
      v-for="it in items"
      :key="it.key"
      class="mi"
      :class="{ dis: it.disabled, danger: it.danger, sep: it.sep }"
      role="menuitem"
      :aria-disabled="it.disabled || undefined"
      @click="onPick(it)"
    >
      <AppIcon v-if="it.icon" :name="it.icon" :size="17" />
      <span>{{ it.label }}</span>
    </div>
  </div>
</template>

<style scoped>
.scrim {
  position: fixed;
  inset: 0;
  z-index: 60;
}
.menu {
  position: fixed;
  z-index: 61;
  min-width: 180px;
  background: var(--card);
  border: 1px solid var(--border);
  border-radius: var(--radius-sm);
  box-shadow: var(--shadow);
  padding: 4px;
  user-select: none;
}
.mi {
  display: flex;
  align-items: center;
  gap: 10px;
  padding: 9px 10px;
  font-size: 13px;
  color: var(--text);
  border-radius: 6px;
  cursor: pointer;
}
.mi:hover {
  background: var(--card-2);
}
.mi.dis {
  color: var(--text-dim);
  opacity: 0.45;
  cursor: default;
}
.mi.dis:hover {
  background: transparent;
}
.mi.danger {
  color: var(--err);
}
.mi.sep {
  margin-top: 4px;
  border-top: 1px solid var(--border);
  border-radius: 0 0 6px 6px;
  padding-top: 11px;
}
</style>
