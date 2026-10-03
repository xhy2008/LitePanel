<script setup lang="ts">
import { computed } from 'vue';
import AppIcon from '../AppIcon.vue';
import { crumbOf, parentOf } from '../../api/files';

// 面包屑（设计 8.1：地址栏支持直接输入路径 + 面包屑分段点击）。
//
// 分段来自 api/files 的 crumbOf —— 视图不自己 split('/')：根目录这一例
// 必须给一个 "/" 段，否则在 /data/x 上没有任何办法跳回根，而手机键盘
// 手打 "/data/x" 的倒数第二段是常态操作。
const props = defineProps<{ path: string }>();
const emit = defineEmits<{ go: [path: string] }>();

const crumbs = computed(() => crumbOf(props.path));
// 根目录的父目录是自身，这时候"上一级"按钮点了也没任何变化 —— 藏掉它,
// 比摆一个按下去不干事的按钮诚实。
const canUp = computed(() => parentOf(props.path) !== props.path);
</script>

<template>
  <div class="bc">
    <div v-if="canUp" class="up" role="button" aria-label="上一级" @click="emit('go', parentOf(path))">
      <AppIcon name="arrow_upward" :size="15" />
    </div>
    <template v-for="(c, i) in crumbs" :key="c.path">
      <span v-if="i > 0" class="sp">/</span>
      <div
        class="seg"
        :class="{ last: i === crumbs.length - 1 }"
        role="button"
        @click="emit('go', c.path)"
      >
        {{ c.label }}
      </div>
    </template>
  </div>
</template>

<style scoped>
.bc {
  display: flex;
  align-items: center;
  gap: 2px;
  font-size: 12px;
  color: var(--text-dim);
  overflow-x: auto;
  scrollbar-width: none;
  white-space: nowrap;
}
.bc::-webkit-scrollbar {
  display: none;
}
.up {
  display: flex;
  padding: 4px;
  border-radius: 5px;
  color: var(--text-dim);
  cursor: pointer;
  flex-shrink: 0;
}
.up:hover {
  background: var(--card-2);
}
.seg {
  padding: 4px 6px;
  border-radius: 5px;
  cursor: pointer;
  flex-shrink: 0;
}
.seg:hover {
  background: var(--card-2);
  color: var(--text);
}
/* 最后一段是当前目录，不可点感（它就是用户已经在的地方）。 */
.seg.last {
  color: var(--text);
  font-weight: 600;
}
.sp {
  color: var(--text-mute);
  flex-shrink: 0;
}
</style>
