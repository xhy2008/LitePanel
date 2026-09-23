<script setup lang="ts">
import { onMounted, ref, watch, nextTick } from 'vue';
import Sheet from './Sheet.vue';
import AppIcon from '../AppIcon.vue';
import { getApi } from '../../api/inject';

// 服务输出抽屉。数据只在后端内存环形缓冲里（D19），这里同样只做
// 有界缓存 —— 两边都不是历史。
const props = withDefaults(
  defineProps<{ id: number; name: string; limit?: number }>(),
  { limit: 500 },
);
const emit = defineEmits<{ close: [] }>();

const lines = ref<string[]>([]);
const box = ref<HTMLElement | null>(null);
const bufferLimit = ref(props.limit);

function append(newLines: string[]) {
  if (!newLines.length) return;
  // 必须有界：无界数组能让跑几天的服务把标签页吃爆，而后端本来就只
  // 保留 500 行，多留的都是自欺欺人。
  lines.value = [...lines.value, ...newLines].slice(-props.limit);
  void nextTick(() => {
    if (box.value) box.value.scrollTop = box.value.scrollHeight;
  });
}

async function load() {
  const { api } = getApi();
  const res = await api.get<{
    lines: string[] | null;
    cached_lines: number;
    buffer_limit: number;
  }>(`/api/services/${props.id}/log?tail=${props.limit}`);
  lines.value = (res.lines ?? []).slice(-props.limit);
  bufferLimit.value = res.buffer_limit || props.limit;
  void nextTick(() => {
    if (box.value) box.value.scrollTop = box.value.scrollHeight;
  });
}

async function clear() {
  const { api } = getApi();
  await api.del(`/api/services/${props.id}/log`);
  lines.value = [];
}

onMounted(load);
watch(() => props.id, load);

defineExpose({ append });
</script>

<template>
  <Sheet :title="`输出 · ${name}`" icon="receipt" @close="emit('close')">
    <div class="lmeta">
      <span class="chip"><AppIcon name="memory" :size="11" /> 内存缓存 · 不持久化</span>
      <span class="chip">最近 {{ bufferLimit }} 行</span>
      <span class="chip">{{ lines.length }} 行已显示</span>
      <button class="btn btn-clear" type="button" @click="clear">清空</button>
    </div>
    <div ref="box" class="logbox">{{ lines.join('\n') }}</div>
  </Sheet>
</template>

<style scoped>
.lmeta {
  display: flex;
  align-items: center;
  gap: 6px;
  margin-bottom: 9px;
  flex-wrap: wrap;
}
.chip {
  font-size: 9.5px;
  color: var(--text-mute);
  border: 1px solid var(--border);
  padding: 2px 7px;
  border-radius: 20px;
  display: inline-flex;
  align-items: center;
  gap: 3px;
}
.btn {
  border: none;
  border-radius: var(--radius-sm);
  padding: 4px 10px;
  font-size: 11px;
  cursor: pointer;
  font-family: inherit;
}
.btn-clear {
  margin-left: auto;
  background: var(--card-2);
  color: var(--text-dim);
}
.logbox {
  background: #0a0d11;
  border: 1px solid var(--border-soft);
  border-radius: var(--radius-sm);
  padding: 10px 12px;
  font-family: ui-monospace, Menlo, Consolas, monospace;
  font-size: 10.5px;
  line-height: 1.6;
  color: #c9d4e0;
  height: 300px;
  overflow-y: auto;
  white-space: pre-wrap;
  word-break: break-all;
}
</style>
