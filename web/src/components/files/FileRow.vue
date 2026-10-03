<script setup lang="ts">
import { onUnmounted } from 'vue';
import AppIcon from '../AppIcon.vue';
import { iconFor, sizeLabel, mtimeLabel } from '../../api/files';
import type { FsEntry } from '../../api/files';

// 文件列表的一行。
//
// 点击行为分成两套，这是手机端文件管理最容易做错的地方：
//   · 行本体点击 = 进入目录（目录）/ 选中（文件）
//   · 左侧圆圈 = 多选勾选
// 手机端如果"单击即打开"，多选就变成不可能（想勾第二个必须先返回），
// 所以圆圈这个独立勾选目标必须存在。
const props = defineProps<{
  entry: FsEntry;
  selected: boolean;
  /** 剪切态（剪贴板里的 cut 项）：整行压暗："这东西要被移走了"。 */
  cut?: boolean;
}>();

const emit = defineEmits<{
  open: [entry: FsEntry];
  toggle: [entry: FsEntry];
  menu: [entry: FsEntry, x: number, y: number];
}>();

// 长按阈值：与移动端惯例一致（~500ms）。太短会让普通点击误弹菜单,
// 太长则不像长按。
const LONG_PRESS_MS = 500;
// 手指移动超过这个距离就认定用户在滚动列表，不是在长按。没有这条,
// 滚动列表时每滑过一行都会弹一个菜单。
const MOVE_TOLERANCE = 10;

let pressTimer: ReturnType<typeof setTimeout> | null = null;
let longFired = false;
let startY = 0;

function clearPress() {
  if (pressTimer) clearTimeout(pressTimer);
  pressTimer = null;
}

// 卸载时也要清：长按后立刻切走页面（点开了菜单又跳转），定时器会在
// 组件销毁之后去 emit 一个已经没人听的菜单事件。
onUnmounted(clearPress);

function onPressStart(e: TouchEvent) {
  // 只在单指时启用长按：双指缩放不该弹菜单。
  if (e.touches.length !== 1) return;
  const t = e.touches[0];
  startY = t.clientY;
  longFired = false;
  pressTimer = setTimeout(() => {
    longFired = true;
    clearPress();
    emit('menu', props.entry, t.clientX, t.clientY);
  }, LONG_PRESS_MS);
}

function onPressMove(e: TouchEvent) {
  if (!pressTimer) return;
  const t = e.touches[0];
  if (!t) return;
  if (Math.abs(t.clientY - startY) > MOVE_TOLERANCE) clearPress();
}

function onClick() {
  // 长按弹菜单后，手指抬起还会附带一个 click：不能再打开文件，否则
  // "想弹菜单"变成"打开了文件"。
  if (longFired) {
    longFired = false;
    return;
  }
  emit('open', props.entry);
}

function onContextMenu(e: MouseEvent) {
  // 不拦住浏览器原生菜单，我们的菜单会叠在它下面。
  e.preventDefault();
  // 必须 stopPropagation：外层列表容器上挂着"空白处右键"的处理器,
  // 而事件会冒泡到它。少了这一行，右键文件会先弹文件菜单、随即被
  // 空白处菜单覆盖掉 —— 表现为"右键文件永远没有重命名和下载",
  // 而两个处理器单独看都完全正确。
  e.stopPropagation();
  emit('menu', props.entry, e.clientX, e.clientY);
}
</script>

<template>
  <div
    class="row"
    :class="{ sel: selected, cut }"
    role="button"
    @click="onClick"
    @contextmenu="onContextMenu"
    @touchstart.passive="onPressStart"
    @touchmove.passive="onPressMove"
    @touchend="clearPress"
    @touchcancel="clearPress"
  >
    <div
      class="chk"
      :class="{ on: selected }"
      role="checkbox"
      :aria-checked="selected"
      @click.stop="emit('toggle', entry)"
    >
      <AppIcon v-if="selected" name="check" :size="13" />
    </div>
    <AppIcon class="ic" :name="iconFor(entry)" :size="20" />
    <div class="nm">
      {{ entry.name }}
      <span v-if="entry.is_symlink" class="lnk">&rarr;</span>
    </div>
    <div class="sz">{{ entry.is_dir ? '' : sizeLabel(entry.size) }}</div>
    <div class="mt">{{ mtimeLabel(entry.mtime) }}</div>
  </div>
</template>

<style scoped>
.row {
  display: flex;
  align-items: center;
  gap: 10px;
  padding: 9px 10px;
  border-radius: 6px;
  cursor: pointer;
  -webkit-tap-highlight-color: transparent;
}
.row.sel {
  background: var(--card-2);
}
.row.cut {
  opacity: 0.45;
}
.chk {
  width: 20px;
  height: 20px;
  flex-shrink: 0;
  border: 1.5px solid var(--border);
  border-radius: 50%;
  display: flex;
  align-items: center;
  justify-content: center;
  color: #fff;
}
.chk.on {
  background: var(--accent);
  border-color: var(--accent);
}
.ic {
  color: var(--text-dim);
}
.nm {
  flex: 1;
  min-width: 0;
  font-size: 13px;
  color: var(--text);
  white-space: nowrap;
  overflow: hidden;
  text-overflow: ellipsis;
}
.lnk {
  color: var(--text-mute);
  font-size: 11px;
}
.sz {
  width: 64px;
  flex-shrink: 0;
  text-align: right;
  font-size: 11px;
  color: var(--text-dim);
  font-variant-numeric: tabular-nums;
}
.mt {
  width: 96px;
  flex-shrink: 0;
  text-align: right;
  font-size: 11px;
  color: var(--text-mute);
  font-variant-numeric: tabular-nums;
}
</style>
