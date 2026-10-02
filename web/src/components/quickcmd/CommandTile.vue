<script setup lang="ts">
import AppIcon from '../AppIcon.vue';
import type { CommandRow } from '../../api/quickcmd';

// 单个快捷命令磁贴（原型 .tile.cmd-tile）。整块可点 = 执行（D20：
// 点击即跳转终端执行，页面底部不再有输出面板）。
//
// 这里刻意没有"忙"角标：忙是**终端会话**的属性，不是命令的属性 ——
// 一条命令会投进哪个会话由后端选（取最近最少使用的空闲会话），前端若在
// 磁贴上标忙闲只能靠猜，猜错的那次就是"看着空闲、点下去却另开了会话"。
// 忙闲标记的正确位置是终端标签；"点了会落到哪"的正确位置是列表顶部提示。
const props = defineProps<{
  row: CommandRow;
  // 在飞标记：重复点同一张磁贴会真的往终端灌两遍命令，对 rm -rf 这类
  // 命令，第二遍是事故不是重复。
  pending?: boolean;
}>();

const emit = defineEmits<{
  run: [id: number];
  edit: [id: number];
  remove: [id: number];
  move: [id: number, dir: 'up' | 'down'];
}>();

function onClick() {
  if (props.pending) return;
  emit('run', props.row.id);
}

// 内部控件的点击不能顺带触发整块执行：点"删除"顺手把命令跑一遍，
// 是这里能犯下的最贵的一次错误。
//
// 注意写法是 @click="stop($event, ...)"，不是 @click="stop(...)"：
// 后者会被 Vue 编成 e => stop(...)，也就是"每次点击都去构造一个处理函数
// 然后丢掉"，看着有绑定、实际一次都不触发。
function stop(e: Event, fn: () => void) {
  e.stopPropagation();
  if (props.pending) return;
  fn();
}
</script>

<template>
  <div class="tile cmd-tile" :class="{ busy: pending }" role="button" @click="onClick">
    <div class="ti" :class="row.need_confirm ? 'ti-danger' : 'ti-run'">
      <AppIcon :name="row.need_confirm ? 'alert' : 'bolt'" :size="18" />
    </div>
    <div class="tinfo">
      <div class="tname">
        {{ row.name }}
        <!-- 危险标记必须在点之前看得见：点完才弹窗，用户就已经站在
             "要不要按确认"的位置上了，那时他才第一次看到这条命令危险。 -->
        <span v-if="row.need_confirm" class="kb kb-danger">确认</span>
      </div>
      <div class="tsub">{{ row.command }}</div>
      <div v-if="row.cwd" class="tcwd">{{ row.cwd }}</div>
    </div>

    <div class="mv">
      <div class="mvi mv-up" role="button" aria-label="上移" @click="stop($event, () => emit('move', row.id, 'up'))">
        <AppIcon name="arrow_upward" :size="14" />
      </div>
      <div class="mvi mv-down" role="button" aria-label="下移" @click="stop($event, () => emit('move', row.id, 'down'))">
        <AppIcon name="arrow_downward" :size="14" />
      </div>
    </div>
    <div class="ib ib-edit" role="button" aria-label="编辑" @click="stop($event, () => emit('edit', row.id))">
      <AppIcon name="edit" :size="16" />
    </div>
    <div class="ib ib-del" role="button" aria-label="删除" @click="stop($event, () => emit('remove', row.id))">
      <AppIcon name="trash" :size="16" />
    </div>
  </div>
</template>

<style scoped>
.cmd-tile {
  display: flex;
  align-items: center;
  gap: 10px;
  padding: 10px;
  background: var(--card);
  border-radius: var(--radius-sm);
  margin-bottom: 8px;
  cursor: pointer;
}
.cmd-tile.busy {
  opacity: 0.55;
  cursor: default;
}
.ti {
  width: 32px;
  height: 32px;
  border-radius: 8px;
  display: flex;
  align-items: center;
  justify-content: center;
  flex: none;
}
.ti-run {
  background: rgba(59, 167, 255, 0.14);
  color: var(--info);
}
.ti-danger {
  background: rgba(255, 77, 79, 0.13);
  color: var(--err);
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
  gap: 6px;
}
.kb {
  font-size: 10px;
  padding: 1px 5px;
  border-radius: 4px;
  font-weight: 500;
}
.kb-danger {
  background: rgba(255, 77, 79, 0.16);
  color: var(--err);
}
.tsub {
  font-size: 11px;
  color: var(--text-mute);
  font-family: var(--mono, ui-monospace, monospace);
  white-space: nowrap;
  overflow: hidden;
  text-overflow: ellipsis;
  margin-top: 2px;
}
.tcwd {
  font-size: 10px;
  color: var(--text-mute);
  margin-top: 2px;
}
.mv {
  display: flex;
  flex-direction: column;
  gap: 2px;
  flex: none;
}
.mvi {
  width: 22px;
  height: 18px;
  display: flex;
  align-items: center;
  justify-content: center;
  border-radius: 4px;
  color: var(--text-mute);
}
.mvi:hover {
  background: rgba(255, 255, 255, 0.06);
  color: var(--text);
}
.ib {
  width: 30px;
  height: 30px;
  border-radius: 7px;
  display: flex;
  align-items: center;
  justify-content: center;
  color: var(--text-dim);
  flex: none;
}
.ib:hover {
  background: rgba(255, 255, 255, 0.06);
  color: var(--text);
}
.ib-del:hover {
  color: var(--err);
}
</style>
