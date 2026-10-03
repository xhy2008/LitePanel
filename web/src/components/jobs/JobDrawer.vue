<script setup lang="ts">
import { computed, onMounted, onUnmounted, ref } from 'vue';
import AppIcon from '../AppIcon.vue';
import Sheet from '../services/Sheet.vue';
import { useFsJobsStore } from '../../stores/fsJobs';
import { attachJobStream } from '../../composables/useJobStream';
import { jobPercent, isActive, OP_LABEL } from '../../api/fsJobs';
import { tryWs } from '../../api/ws';
import type { WsClient } from '../../api/ws';
import type { JobRow } from '../../api/fsJobs';

// 全局任务抽屉（设计 404 行）：任何页面右下角可唤起，显示进行中/已完成
// 任务与进度条。
//
// 挂在壳层而不是文件页里：任务的生命周期与"用户当前看哪个页"无关 ——
// 提交复制后切到终端页是必然会发生的事，抽屉跟着文件页卸载就等于把
// M6-T4 整套"关浏览器也不中断"的后端能力在界面上抹掉了（看不见 = 以为没了
// = 会去重做一遍，而那正是队列要防的重复劳动）。
const props = defineProps<{ wsClient?: WsClient }>();

const store = useFsJobsStore();
const open = ref(false);
const busy = ref<Record<number, boolean>>({});

function client(): WsClient | null {
  return props.wsClient ?? tryWs();
}

let detach: (() => void) | null = null;

onMounted(async () => {
  // 先拉一次再订：推送只发变更，冷启动时列表里已有的任务（上一次会话
  // 留下的、或别的标签页提交的）不推就永远看不见。
  //
  // 取不到依赖就整块跳过，与 App.vue 接 metrics 时同一写法：抽屉是壳层
  // 上的挂件，api / ws 没装配起来时它不该把 mounted 钩子抛出去（未捕获的
  // mounted 异常在 Vue 里只是一条 stderr，而挂载失败的组件会把同层其他
  // 挂件一起带走）。真正该报错的是启动流程本身，那里有它自己的报错口。
  try {
    await store.load();
  } catch {
    return;
  }
  const c = client();
  if (c) detach = attachJobStream({ store, wsClient: c });
});

onUnmounted(() => {
  detach?.();
  detach = null;
});

const badge = computed(() => store.activeCount);

// 进行中排前面：抽屉的核心用途是"现在在跑什么"，翻到底下找进行中
// 等于没有。同组内按 id 倒序（新提交在前）。
const sorted = computed(() =>
  [...store.items].sort((a, b) => {
    const d = Number(isActive(b.state)) - Number(isActive(a.state));
    return d !== 0 ? d : b.id - a.id;
  }),
);

function pct(j: JobRow) {
  return jobPercent(j);
}

function label(j: JobRow): string {
  switch (j.state) {
    case 'pending':
      return j.cancel_requested ? '排队中 · 将取消' : '排队中';
    case 'running':
      // cancel_requested 单独成态：worker 还要几个块才停得下来，这时候
      // 写"进行中"是假话，写"已取消"更是 —— 文件还在被删。
      return j.cancel_requested ? '正在取消…' : '进行中';
    case 'done':
      return '已完成';
    case 'failed':
      return '失败';
    case 'canceled':
      return '已取消';
    case 'interrupted':
      return '已中断';
    default:
      return j.state;
  }
}

function tone(j: JobRow): string {
  switch (j.state) {
    case 'done':
      return 'good';
    case 'failed':
    case 'interrupted':
      return 'bad';
    case 'canceled':
      return 'mute';
    default:
      return 'run';
  }
}

/** 百分比文字。null = 分母未知，画不出也不该编一个数。 */
function pctText(j: JobRow): string {
  const p = pct(j);
  return p === null ? '' : p + '%';
}

function icon(op: JobRow['op']): string {
  return op === 'copy' ? 'copy' : op === 'move' ? 'move' : 'trash';
}

/** 一句话摘要：条目数比字节可靠（total_bytes 后端恒为 0）。 */
function detail(j: JobRow): string {
  const n = j.entries_total > 0 ? `${j.entries_done}/${j.entries_total} 项` : `${j.entries_done} 项`;
  if (j.op === 'delete') return n;
  return j.dst ? `${n} → ${j.dst}` : n;
}

async function onCancel(j: JobRow) {
  if (busy.value[j.id]) return;
  busy.value = { ...busy.value, [j.id]: true };
  try {
    await store.cancel(j.id);
  } catch {
    /* 错误已进 store.error */
  } finally {
    const rest = { ...busy.value };
    delete rest[j.id];
    busy.value = rest;
  }
}

async function onRetry(j: JobRow) {
  if (busy.value[j.id]) return;
  busy.value = { ...busy.value, [j.id]: true };
  try {
    await store.retry(j.id);
  } catch {
    /* 同上 */
  } finally {
    const rest = { ...busy.value };
    delete rest[j.id];
    busy.value = rest;
  }
}
</script>

<template>
  <button
    class="job-fab"
    :class="{ idle: badge === 0 }"
    :aria-label="`任务（${badge} 个进行中）`"
    @click="open = !open"
  >
    <AppIcon name="receipt" :size="20" />
    <span v-if="badge > 0" class="job-badge">{{ badge }}</span>
  </button>

  <Sheet v-if="open" title="任务" icon="receipt" @close="open = false">
    <div v-if="sorted.length === 0" class="job-empty">
      <AppIcon name="receipt" :size="28" />
      <p>还没有任务</p>
      <small>复制 / 移动 / 删除都会出现在这里，关掉浏览器也不会中断。</small>
    </div>

    <ul v-else class="job-list">
      <li v-for="j in sorted" :key="j.id" class="job-row" :data-state="j.state">
        <div class="job-head">
          <span class="job-ico" :class="tone(j)"><AppIcon :name="icon(j.op)" :size="15" /></span>
          <span class="job-title">{{ OP_LABEL[j.op] }}</span>
          <span class="job-state" :class="tone(j)">{{ label(j) }}</span>
        </div>

        <div class="job-bar">
          <div
            v-if="pct(j) !== null"
            class="job-fill"
            :class="tone(j)"
            :style="{ width: pct(j) + '%' }"
          />
        </div>

        <div class="job-meta">
          <span class="job-detail">{{ detail(j) }}</span>
          <!-- 数字必须写出来，不能只靠条的宽度：色条 + 宽度对读屏是不可见的，
               而"进行中"三个字给不出还要等多久。 -->
          <span v-if="pctText(j)" class="job-pct">{{ pctText(j) }}</span>
          <!-- 后端错误原文必须露出来：只写"失败"两个字，用户唯一能做的
               就是重跑一遍，而如果原因是"磁盘满了"，重跑只是再失败一次。 -->
          <span v-if="j.error" class="job-error" :title="j.error">{{ j.error }}</span>
        </div>

        <div class="job-acts">
          <button
            v-if="isActive(j.state) && !j.cancel_requested"
            class="job-cancel"
            :disabled="!!busy[j.id]"
            @click="onCancel(j)"
          >
            取消
          </button>
          <button v-if="j.state === 'interrupted'" class="job-retry" :disabled="!!busy[j.id]" @click="onRetry(j)">
            重试
          </button>
        </div>
      </li>
    </ul>
  </Sheet>
</template>

<style scoped>
.job-fab {
  position: fixed;
  right: 16px;
  bottom: calc(var(--tabbar-h, 56px) + 16px);
  z-index: 150;
  width: 44px;
  height: 44px;
  border-radius: 50%;
  border: 1px solid var(--border);
  background: var(--card);
  color: var(--accent);
  display: flex;
  align-items: center;
  justify-content: center;
  cursor: pointer;
}
/* 没有进行中任务时压暗：一个永远鲜亮的角标会让人以为一直在忙。 */
.job-fab.idle {
  color: var(--text-mute);
}
.job-badge {
  position: absolute;
  top: -4px;
  right: -4px;
  min-width: 17px;
  height: 17px;
  padding: 0 4px;
  border-radius: 9px;
  background: var(--accent);
  color: #fff;
  font-size: 11px;
  line-height: 17px;
  text-align: center;
}
.job-list {
  list-style: none;
  margin: 0;
  padding: 0;
  display: flex;
  flex-direction: column;
  gap: 10px;
}
.job-row {
  background: var(--card);
  border: 1px solid var(--border-soft);
  border-radius: var(--radius-md);
  padding: 10px 12px;
}
.job-head {
  display: flex;
  align-items: center;
  gap: 8px;
}
.job-ico {
  width: 22px;
  height: 22px;
  border-radius: 6px;
  display: flex;
  align-items: center;
  justify-content: center;
  background: var(--card-2);
}
.job-title {
  font-size: 13px;
  color: var(--text);
  flex: 1;
}
.job-state {
  font-size: 12px;
  color: var(--text-dim);
}
.job-bar {
  height: 4px;
  border-radius: 2px;
  background: var(--card-2);
  margin: 8px 0 6px;
  overflow: hidden;
}
.job-fill {
  height: 100%;
  border-radius: 2px;
  transition: width 0.25s ease;
}
.job-meta {
  display: flex;
  flex-direction: column;
  gap: 2px;
  font-size: 11px;
  color: var(--text-mute);
  min-width: 0;
}
.job-detail {
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
}
.job-error {
  color: var(--err);
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
}
.job-acts {
  display: flex;
  gap: 8px;
  margin-top: 8px;
}
.job-cancel,
.job-retry {
  font-size: 12px;
  padding: 4px 12px;
  border-radius: var(--radius-sm);
  border: 1px solid var(--border);
  background: transparent;
  color: var(--text-dim);
  cursor: pointer;
}
.job-retry {
  color: var(--accent);
  border-color: var(--accent);
}
.job-cancel:disabled,
.job-retry:disabled {
  opacity: 0.5;
  cursor: default;
}
.job-empty {
  text-align: center;
  padding: 28px 12px;
  color: var(--text-mute);
  display: flex;
  flex-direction: column;
  align-items: center;
  gap: 6px;
}
.job-empty p {
  margin: 0;
  font-size: 13px;
  color: var(--text-dim);
}
.job-empty small {
  font-size: 11px;
  max-width: 260px;
  line-height: 1.5;
}
.run {
  color: var(--info);
}
.good {
  color: var(--ok);
}
.bad {
  color: var(--err);
}
.mute {
  color: var(--text-mute);
}
.job-fill.good {
  background: var(--ok);
}
.job-fill.run {
  background: var(--info);
}
.job-fill.bad {
  background: var(--err);
}
</style>
