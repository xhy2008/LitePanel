<script setup lang="ts">
import { onMounted, onUnmounted, ref, watch } from 'vue';
import ServiceTile from './ServiceTile.vue';
import LogSheet from './LogSheet.vue';
import ServiceForm from './ServiceForm.vue';
import AppIcon from '../AppIcon.vue';
import { useServicesStore } from '../../stores/services';
import { attachServiceStream } from '../../composables/useServiceStream';
import { tryWs } from '../../api/ws';
import type { WsClient } from '../../api/ws';

// 服务管理面板（快捷命令标签页的第一个分段）。列表与右栏指标共用同一条
// WS 连接，按频道多路复用。
const props = defineProps<{ wsClient?: WsClient }>();

const store = useServicesStore();
const formFor = ref<number | 'new' | null>(null);
const logFor = ref<{ id: number; name: string } | null>(null);

// 没有 WS 也照常渲染：列表、表单、日志快照都走 HTTP，只是不实时。
function client(): WsClient | null {
  return props.wsClient ?? tryWs();
}

let detach: (() => void) | null = null;
let detachLog: (() => void) | null = null;

// 只声明用到的那一个方法：用 InstanceType<typeof LogSheet> 会把整个组件
// 内部实现拖进这里的类型，改组件内部就会连带影响父层类型。
interface LogSheetHandle {
  append: (lines: string[]) => void;
}
const logSheet = ref<LogSheetHandle | null>(null);

// 日志频道的订阅跟着抽屉走：开着才订。抽屉关着还挂着订阅，
// 每次推送都在给一个看不见的组件拼字符串，开合十次就攒下十份。
watch(logFor, (target) => {
  detachLog?.();
  detachLog = null;
  if (!target) return;
  const c = client();
  if (!c) return;
  detachLog = c.subscribe(`svclog:${target.id}`, (data) => {
    const payload = data as { lines?: string[] };
    if (payload?.lines) logSheet.value?.append(payload.lines);
  });
});

onMounted(async () => {
  const c = client();
  if (c) detach = attachServiceStream({ store, wsClient: c });
  await store.load();
});

onUnmounted(() => {
  detach?.();
  detachLog?.();
  detach = null;
  detachLog = null;
});

async function onSaved() {
  formFor.value = null;
  await store.load();
}

async function onDeleted() {
  if (typeof formFor.value !== 'number') return;
  const id = formFor.value;
  formFor.value = null;
  if (logFor.value?.id === id) logFor.value = null;
  await store.remove(id);
}

const editingRow = () =>
  typeof formFor.value === 'number'
    ? store.items.find((i) => i.id === formFor.value)
    : undefined;

function setSheet(el: unknown) {
  logSheet.value = (el as LogSheetHandle | null) ?? null;
}

function openLog(id: number) {
  const row = store.items.find((i) => i.id === id);
  logFor.value = { id, name: row?.name ?? String(id) };
}
</script>

<template>
  <div class="svc">
    <div v-if="store.error" class="errline">{{ store.error }}</div>

    <ServiceTile
      v-for="item in store.items"
      :key="item.id"
      :row="item"
      :busy="!!store.pending[item.id]"
      @toggle="store.toggle($event)"
      @log="openLog($event)"
      @edit="formFor = $event"
    />

    <div class="tile tile-add" role="button" @click="formFor = 'new'">
      <AppIcon name="add" :size="16" /> 添加服务
    </div>

    <div class="hintbar">
      <AppIcon name="info" :size="14" />
      <span>CMD 类型服务随面板存活，面板重启会被一并停止。SYSV 类型不受影响。</span>
    </div>

    <ServiceForm
      v-if="formFor !== null"
      class="svc-form"
      :editing="editingRow()"
      @saved="onSaved"
      @close="formFor = null"
      @deleted="onDeleted"
    />

    <LogSheet
      v-if="logFor"
      :id="logFor.id"
      :name="logFor.name"
      :ref="setSheet"
      @close="logFor = null"
    />
  </div>
</template>

<style scoped>
.svc {
  padding: 2px 0;
}
.tile-add {
  background: transparent;
  border: 1px dashed var(--border);
  border-radius: var(--radius-md);
  padding: 10px 12px;
  min-height: 56px;
  margin-bottom: 7px;
  display: flex;
  align-items: center;
  justify-content: center;
  gap: 5px;
  color: var(--text-mute);
  font-size: 12px;
  cursor: pointer;
}
.hintbar {
  display: flex;
  align-items: center;
  gap: 6px;
  font-size: 10px;
  color: var(--text-mute);
  padding: 9px 12px;
  background: rgba(245, 166, 35, 0.07);
  border: 1px solid rgba(245, 166, 35, 0.2);
  border-radius: var(--radius-sm);
  margin-top: 10px;
  line-height: 1.5;
}
.hintbar :deep(svg) {
  color: var(--warn);
  flex-shrink: 0;
}
.errline {
  font-size: 11.5px;
  color: var(--err);
  padding: 8px 10px;
  margin-bottom: 8px;
  background: rgba(255, 77, 79, 0.08);
  border: 1px solid rgba(255, 77, 79, 0.22);
  border-radius: var(--radius-sm);
}
</style>
