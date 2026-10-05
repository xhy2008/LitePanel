<script setup lang="ts">
import { computed, onMounted, onUnmounted, ref } from 'vue';
import { tryWs, type WsClient } from '../api/ws';
import { displayName, speedLabel, type DlEvent, type DlTask } from '../api/downloads';
import { DL_POLL_MS, useDownloadsStore } from '../stores/downloads';
import Sheet from '../components/services/Sheet.vue';

// wsClient 走 prop 注入（与 ServicesPanel 同一约定）而不是在测试里
// 改全局单例：测试能真正验证"事件接进了 store"，而不是测一个替身全局。
const props = defineProps<{ wsClient?: WsClient }>();

// 下载页（M7-T4，IDM 风格）。
//
// 刷新策略是"事件为主、轮询兜底"：aria2 没有进度事件（设计 751），事件只报
// 状态变化，进度百分比全靠拉。WS 断线期间界面绝不能静止不动 —— 3 秒轮询
// 保底。这与 services 页一致，但进度条让轮询在这里更必要而不是更奢侈。
//
// aria2 掉线不白屏：health.ok=false 出横幅，列表照常展示历史任务（它们
// 存在面板自己的库里，不依赖 aria2 活着）。
const store = useDownloadsStore();

let detachWs: (() => void) | null = null;
let poll: ReturnType<typeof setInterval> | null = null;

onMounted(() => {
  void store.load();
  const c = props.wsClient ?? tryWs();
  if (c) {
    detachWs = c.subscribe('downloads', (d) => store.applyEvent(d as DlEvent));
  }
  poll = setInterval(() => {
    // 拉取失败静默：下一轮还会试，反复弹错误横幅比短暂数据陈旧更糟。
    void store.load().catch(() => {});
  }, DL_POLL_MS);
});

onUnmounted(() => {
  detachWs?.();
  if (poll) clearInterval(poll);
});

// ---- 分组折叠状态 ----
const showDone = ref(false);
const showFailed = ref(false);

const counts = computed(() => ({
  active: store.active.length,
  paused: store.paused.length,
  done: store.done.length,
  failed: store.failed.length,
}));

// ---- 新建下载对话框 ----
const addOpen = ref(false);
const urisText = ref('');
const dirText = ref('');
const outText = ref('');
const addErr = ref('');

function openAdd() {
  addErr.value = '';
  store.actionError = '';
  addOpen.value = true;
}

async function submitAdd() {
  addErr.value = '';
  // 一行一个地址：多镜像/多文件是 aria2 的正常用法，逐行拆比再来一个
  // "添加地址"按钮少一半点击。空行直接丢。
  const uris = urisText.value
    .split('\n')
    .map((s) => s.trim())
    .filter(Boolean);
  if (!uris.length) {
    addErr.value = '请至少填写一个下载地址';
    return;
  }
  const ok = await store.add({
    uris,
    dir: dirText.value.trim() || undefined,
    out: outText.value.trim() || undefined,
  });
  if (ok) {
    addOpen.value = false;
    urisText.value = '';
    dirText.value = '';
    outText.value = '';
  }
}

// ---- 行操作 ----
function fmtBytes(n: number): string {
  if (n <= 0) return '0 B';
  const units = ['B', 'KB', 'MB', 'GB', 'TB'];
  let v = n;
  let i = 0;
  while (v >= 1024 && i < units.length - 1) {
    v /= 1024;
    i++;
  }
  return `${v >= 100 || i === 0 ? Math.round(v) : v.toFixed(1)} ${units[i]}`;
}

function pct(t: DlTask): number {
  if (t.state === 'complete') return 100;
  if (t.total_bytes <= 0) return 0;
  return Math.min(100, Math.round((t.done_bytes / t.total_bytes) * 100));
}

const confirmRemove = ref<DlTask | null>(null);
const removeForce = ref(false);
async function doRemove() {
  const t = confirmRemove.value;
  confirmRemove.value = null;
  if (t) await store.remove(t.gid, removeForce.value);
}

function askRemove(t: DlTask) {
  removeForce.value = false;
  confirmRemove.value = t;
}

const clearing = ref(false);
async function doClear() {
  clearing.value = true;
  await store.clearHist();
  clearing.value = false;
}
</script>

<template>
  <div class="view dl">
    <!-- aria2 掉线横幅：必须比任何列表显眼，否则用户会把"全部暂停不动"
         当成面板坏了。文案用后端给的 message（它知道失败原因）。 -->
    <div v-if="store.health && !store.health.ok" class="dl-alert">
      aria2 不可用：{{ store.health.message || '未知原因' }}。历史任务仍可见，
      新任务与控制操作会失败。
    </div>

    <div v-if="store.error && !store.loaded" class="dl-err">{{ store.error }}</div>
    <div v-else-if="store.loading && !store.loaded" class="dl-hint">载入下载任务…</div>

    <template v-else>
      <!-- 顶栏：全局速度 + 动作 -->
      <div class="dl-top">
        <div class="dl-stat">
          <span v-if="store.summary" class="dl-speed">
            ↓ {{ speedLabel(store.summary.speed) || '0 B/s' }}
          </span>
          <span class="dl-counts">
            {{ counts.active }} 下载中 · {{ counts.paused }} 已暂停
          </span>
        </div>
        <button class="dl-btn primary" @click="openAdd">新建下载</button>
      </div>

      <p v-if="store.actionError" class="dl-err">{{ store.actionError }}</p>

      <!-- 下载中 -->
      <section v-if="counts.active" class="dl-sec">
        <h3>下载中 ({{ counts.active }})</h3>
        <div v-for="t in store.active" :key="t.gid" class="dl-row">
          <div class="dl-r1">
            <span class="dl-name">{{ displayName(t) }}</span>
            <span class="dl-meta">
              {{ t.state === 'waiting' ? '排队中' : speedLabel(t.speed) }}
              <template v-if="t.connections"> · {{ t.connections }} 连接</template>
            </span>
          </div>
          <div class="dl-bar">
            <div class="dl-fill" :style="{ width: pct(t) + '%' }" />
          </div>
          <div class="dl-r2">
            <span>{{ pct(t) }}% · {{ fmtBytes(t.done_bytes) }}<template v-if="t.total_bytes"> / {{ fmtBytes(t.total_bytes) }}</template></span>
            <span class="dl-acts">
              <button
                v-if="t.state === 'active' && t.can_control"
                class="dl-btn sm"
                @click="store.pause(t.gid)"
              >暂停</button>
              <button class="dl-btn sm" :disabled="!t.can_control" @click="askRemove(t)">删除</button>
            </span>
          </div>
        </div>
      </section>

      <!-- 已暂停 -->
      <section v-if="counts.paused" class="dl-sec">
        <h3>已暂停 ({{ counts.paused }})</h3>
        <div v-for="t in store.paused" :key="t.gid" class="dl-row">
          <div class="dl-r1">
            <span class="dl-name">{{ displayName(t) }}</span>
            <span class="dl-meta">{{ pct(t) }}%</span>
          </div>
          <div class="dl-bar">
            <div class="dl-fill pause" :style="{ width: pct(t) + '%' }" />
          </div>
          <div class="dl-r2">
            <span>{{ fmtBytes(t.done_bytes) }}<template v-if="t.total_bytes"> / {{ fmtBytes(t.total_bytes) }}</template></span>
            <span class="dl-acts">
              <button class="dl-btn sm" :disabled="!t.can_control" @click="store.resume(t.gid)">继续</button>
              <button class="dl-btn sm" :disabled="!t.can_control" @click="askRemove(t)">删除</button>
            </span>
          </div>
        </div>
      </section>

      <!-- 失败 -->
      <section v-if="counts.failed" class="dl-sec">
        <h3 class="dl-fail-h" @click="showFailed = !showFailed">
          失败 ({{ counts.failed }}) <span class="dl-caret">{{ showFailed ? '▾' : '▸' }}</span>
        </h3>
        <template v-if="showFailed">
          <div v-for="t in store.failed" :key="t.gid" class="dl-row">
            <div class="dl-r1">
              <span class="dl-name">{{ displayName(t) }}</span>
            </div>
            <!-- 失败原因原文直出（设计硬性要求：不许只报"失败"） -->
            <p v-if="t.error" class="dl-rerr">{{ t.error }}</p>
            <div class="dl-r2">
              <span>{{ t.dir }}</span>
              <span class="dl-acts">
                <button class="dl-btn sm" :disabled="!t.can_control" @click="store.resume(t.gid)">重试</button>
                <button class="dl-btn sm" :disabled="!t.can_control" @click="askRemove(t)">删除</button>
              </span>
            </div>
          </div>
        </template>
      </section>

      <!-- 完成 -->
      <section v-if="counts.done" class="dl-sec">
        <h3 class="dl-fail-h" @click="showDone = !showDone">
          已完成 ({{ counts.done }}) <span class="dl-caret">{{ showDone ? '▾' : '▸' }}</span>
        </h3>
        <template v-if="showDone">
          <div v-for="t in store.done" :key="t.gid" class="dl-row">
            <div class="dl-r1">
              <span class="dl-name">{{ displayName(t) }}</span>
              <span class="dl-meta">{{ t.total_bytes ? fmtBytes(t.total_bytes) : '' }}</span>
            </div>
            <div class="dl-r2">
              <span>{{ t.dir }}</span>
              <span class="dl-acts">
                <button class="dl-btn sm" :disabled="!t.can_control" @click="askRemove(t)">删除</button>
              </span>
            </div>
          </div>
          <button class="dl-btn" :disabled="clearing" @click="doClear">清空历史</button>
        </template>
      </section>

      <p v-if="!counts.active && !counts.paused && !counts.failed && !counts.done" class="dl-hint">
        没有下载任务。点"新建下载"添加一个。
      </p>
    </template>

    <!-- 新建下载 -->
    <Sheet v-if="addOpen" title="新建下载" @close="addOpen = false">
      <label class="dl-f">下载地址（每行一个，可填镜像）
        <textarea v-model="urisText" rows="3" placeholder="https://example.com/file.zip" />
      </label>
      <label class="dl-f">保存目录（留空用默认）
        <input v-model="dirText" type="text" placeholder="/data/downloads" />
      </label>
      <label class="dl-f">另存为（可选）
        <input v-model="outText" type="text" placeholder="改个文件名" />
      </label>
      <p v-if="addErr" class="dl-err">{{ addErr }}</p>
      <template #footer>
        <button class="dl-btn" @click="addOpen = false">取消</button>
        <button class="dl-btn primary" :disabled="store.busy" @click="submitAdd">
          {{ store.busy ? '提交中…' : '开始下载' }}
        </button>
      </template>
    </Sheet>

    <!-- 删除确认 -->
    <Sheet v-if="confirmRemove" title="删除下载任务？" @close="confirmRemove = null">
      <p>从列表移除「{{ confirmRemove ? displayName(confirmRemove) : '' }}」。</p>
      <label class="dl-ck">
        <input v-model="removeForce" type="checkbox" />
        同时删除已下载的部分文件（aria2 强制删除，可能残留 .aria2 控制文件）
      </label>
      <template #footer>
        <button class="dl-btn" @click="confirmRemove = null">取消</button>
        <button class="dl-btn danger" @click="doRemove">删除</button>
      </template>
    </Sheet>
  </div>
</template>

<style scoped>
.dl {
  padding: 12px;
  max-width: 860px;
  margin: 0 auto;
}
.dl-alert {
  background: #3a2410;
  border: 1px solid #6b4a1f;
  color: #f0c674;
  border-radius: 8px;
  padding: 9px 12px;
  font-size: 13px;
  margin-bottom: 12px;
}
.dl-err {
  color: #ff7a7a;
  font-size: 13px;
  margin: 6px 0;
}
.dl-hint {
  color: #8a8a9a;
  font-size: 13px;
}
.dl-top {
  display: flex;
  justify-content: space-between;
  align-items: center;
  margin-bottom: 12px;
  gap: 10px;
}
.dl-stat {
  display: flex;
  flex-direction: column;
  gap: 2px;
}
.dl-speed {
  color: #7fe0a6;
  font-size: 14px;
}
.dl-counts {
  color: #8a8a9a;
  font-size: 12px;
}
.dl-sec {
  margin-bottom: 16px;
}
.dl-sec h3 {
  font-size: 13px;
  color: #b9b9c7;
  margin: 0 0 8px;
  cursor: default;
}
.dl-fail-h {
  cursor: pointer;
}
.dl-caret {
  color: #6a6a7a;
}
.dl-row {
  background: #16161d;
  border: 1px solid #23232d;
  border-radius: 8px;
  padding: 9px 11px;
  margin-bottom: 8px;
}
.dl-r1 {
  display: flex;
  justify-content: space-between;
  gap: 10px;
  align-items: baseline;
}
.dl-name {
  font-size: 13px;
  color: #e6e6f0;
  overflow-wrap: anywhere;
}
.dl-meta {
  font-size: 12px;
  color: #8a8a9a;
  white-space: nowrap;
}
.dl-bar {
  height: 5px;
  background: #22222c;
  border-radius: 3px;
  margin: 7px 0 5px;
  overflow: hidden;
}
.dl-fill {
  height: 100%;
  background: #2b7fd7;
  border-radius: 3px;
  transition: width 0.4s;
}
.dl-fill.pause {
  background: #6a6a7a;
}
.dl-r2 {
  display: flex;
  justify-content: space-between;
  align-items: center;
  gap: 10px;
  font-size: 12px;
  color: #8a8a9a;
}
.dl-acts {
  display: flex;
  gap: 6px;
}
.dl-rerr {
  color: #ff9a9a;
  font-size: 12px;
  margin: 5px 0 0;
  overflow-wrap: anywhere;
}
.dl-btn {
  background: #22222c;
  color: #d6d6e2;
  border: 1px solid #2f2f3b;
  border-radius: 7px;
  padding: 6px 12px;
  font-size: 13px;
  cursor: pointer;
}
.dl-btn.sm {
  padding: 3px 9px;
  font-size: 12px;
}
.dl-btn.primary {
  background: #2b5bd7;
  border-color: #2b5bd7;
  color: #fff;
}
.dl-btn.danger {
  background: #7a2430;
  border-color: #7a2430;
  color: #fff;
}
.dl-btn:disabled {
  opacity: 0.5;
  cursor: default;
}
.dl-f {
  display: block;
  font-size: 13px;
  color: #b9b9c7;
  margin-bottom: 10px;
}
.dl-f input,
.dl-f textarea {
  display: block;
  width: 100%;
  margin-top: 4px;
  background: #101016;
  border: 1px solid #2a2a36;
  color: #e6e6f0;
  border-radius: 7px;
  padding: 7px 9px;
  font-size: 13px;
  font-family: inherit;
  resize: vertical;
}
.dl-ck {
  display: flex;
  gap: 7px;
  align-items: flex-start;
  font-size: 12px;
  color: #b9b9c7;
}
</style>
