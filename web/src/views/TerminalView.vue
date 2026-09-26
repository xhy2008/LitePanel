<script setup lang="ts">
import { computed, nextTick, onMounted, onUnmounted, ref, watch } from 'vue';
import { Terminal } from '@xterm/xterm';
import { FitAddon } from '@xterm/addon-fit';
import '@xterm/xterm/css/xterm.css';
import AppIcon from '../components/AppIcon.vue';
import Sheet from '../components/services/Sheet.vue';
import { useTerminalStore, autoTitle } from '../stores/terminal';
import { createTerminalRuntime } from '../composables/useTerminal';
import { useTermRuntimes, type Runtime } from '../composables/useTermRuntimes';
import { termChannel, HISTORY_LIMITS, DEFAULT_HISTORY_LIMIT } from '../api/terminal';
import { useBreakpoint } from '../composables/useBreakpoint';
import { tryWs, type WsClient } from '../api/ws';

// 终端视图（设计 7.4）。同一时刻只保留**一个** xterm 实例：每个实例都带着
// 屏幕缓冲、一条 WS 订阅和一个尺寸轮询，留着"看过的全部"就是留着 N 份
// 输出在往浏览器灌，而用户只看得到一个。切回来的画面由服务端重放补齐
// （tmux 的 history-limit 才是历史的唯一归属，前端 scrollback 特意设 0）。
const props = defineProps<{ wsClient?: WsClient }>();

const store = useTerminalStore();
const { bp } = useBreakpoint();
const isPhone = computed(() => bp.value === 'phone');

interface TermRuntime extends Runtime {
  id: number;
  checkSize(): void;
  onOpen(): void;
}

const hosts = new Map<number, HTMLElement>();
const wsStatus = ref<'connecting' | 'online' | 'offline'>('offline');
const dropped = ref(0);
const fullscreen = ref(false);
const busy = computed(() => store.loading || store.creating);

// 抽屉
const formOpen = ref(false);
const form = ref({ title: '', cwd: '', history: DEFAULT_HISTORY_LIMIT });
const confirmDelete = ref<{ id: number; title: string } | null>(null);
const renameTo = ref<{ id: number; title: string } | null>(null);

let client: WsClient | null = null;
let tick: ReturnType<typeof setInterval> | null = null;
let offStatus: (() => void) | null = null;

function setPane(id: number, el: unknown) {
  if (el) hosts.set(id, el as HTMLElement);
  else hosts.delete(id);
}

function makeRuntime(id: number): TermRuntime {
  const parent = hosts.get(id);
  if (!parent || !client) {
    // 容器还没挂载（首次进入、或会话刚被删除）时不能建实例。
    // 这里返回一个空壳，下一次轮询会重新建真的。
    return lazyShell(id);
  }
  const term = new Terminal({
    cursorBlink: true,
    cursorStyle: 'bar',
    // 历史只归 tmux：前端再留一份 scrollback，跨设备看到的就不是同一段
    // 滚动区，而且两份会各自被不同的窗口尺寸重新折行。
    scrollback: 0,
    fontSize: isPhone.value ? 13 : 14,
    fontFamily: "ui-monospace, SFMono-Regular, 'Roboto Mono', Menlo, monospace",
    macOptionIsMeta: true,
    theme: readTheme(),
  });
  const fit = new FitAddon();
  term.loadAddon(fit);
  term.open(parent);

  const channel = termChannel(id);
  const rt = createTerminalRuntime({
    term,
    fit,
    channel,
    send: (p) => client!.sendTerm(channel, p),
    connected: () => client?.isOpen() ?? false,
    onDropped: (n) => {
      dropped.value = n;
    },
  });
  rt.attach();

  const unsub = client.subscribeTerm(channel, (p) => rt.onPayload(p));
  const statusOff = client.onStatus((s) => {
    wsStatus.value = s as typeof wsStatus.value;
    if (s === 'online') rt.onOpen(); // 只重报尺寸，绝不补发断线期间的按键
  });

  return {
    id,
    checkSize: rt.checkSize,
    onOpen: rt.onOpen,
    dispose() {
      unsub();
      statusOff();
      rt.dispose();
    },
  };
}

// 容器没就绪时的占位：不建终端，但也不能让上层以为"没有实例"而
// 每 250ms 重建一次壳 —— 那会一直空转。
function lazyShell(id: number): TermRuntime {
  pending.add(id);
  return {
    id,
    checkSize() {
      if (!pending.delete(id)) return;
      // 容器这次多半已经挂上了：换掉自己。
      runtimes.focus(id);
    },
    onOpen() {},
    dispose() {
      pending.delete(id);
    },
  };
}

const pending = new Set<number>();
const runtimes = useTermRuntimes<TermRuntime>({ make: makeRuntime });

const activeId = computed(() => store.activeId);
const activeRow = computed(() => store.active);
const offline = computed(() => wsStatus.value !== 'online');
const suggestion = computed(() => autoTitle(form.value.title, store.sessions));

// 当前会话（或它死了之后顶上的那一个）换了，就要有实例。
async function syncActive() {
  await nextTick();
  const cur = activeRow.value;
  if (!cur) {
    runtimes.focus(0);
    return;
  }
  if (!cur.alive) {
    runtimes.markDead(cur.id);
    return;
  }
  runtimes.focus(cur.id);
}

watch(activeId, syncActive);

// 存活状态变了（reload 之后才看得出"已经在 tmux 里退出了"）：
// 屏幕必须跟着回收，否则用户对着上一次的画面以为程序还在跑。
watch(
  () => store.sessions.map((s) => `${s.id}:${s.alive}`).join(','),
  syncActive,
);

async function pick(id: number) {
  if (id === store.activeId) return;
  store.select(id);
  await syncActive();
}

async function createSession() {
  try {
    await store.create({
      title: form.value.title,
      cwd: form.value.cwd.trim() || undefined,
      history_limit: form.value.history,
    });
    formOpen.value = false;
    form.value = { title: '', cwd: '', history: DEFAULT_HISTORY_LIMIT };
    await syncActive();
  } catch {
    /* 文案在 store.error 里，抽屉保持打开让用户改 */
  }
}

async function doRename() {
  if (!renameTo.value) return;
  const { id, title } = renameTo.value;
  renameTo.value = null;
  try {
    await store.rename(id, title);
  } catch {
    /* store.error */
  }
}

async function doDelete() {
  if (!confirmDelete.value) return;
  const id = confirmDelete.value.id;
  confirmDelete.value = null;
  try {
    await store.remove(id);
  } catch {
    return;
  }
  runtimes.remove(id, store.activeId);
  await syncActive();
}

function readTheme() {
  // 终端配色跟着面板主题走。深色底上把前景写成浅色是刚需：
  // 兜底值一旦反了，浅色主题下就是白字白底，什么都看不见。
  const v = (name: string, fallback: string) => {
    const got =
      typeof document === 'undefined'
        ? ''
        : getComputedStyle(document.documentElement).getPropertyValue(name).trim();
    return got || fallback;
  };
  return {
    background: v('--card', '#191f27'),
    foreground: v('--text', '#e6ebf2'),
    cursor: v('--accent', '#ff6600'),
    selectionBackground: v('--accent', '#ff6600') + '55',
  };
}

onMounted(async () => {
  client = props.wsClient ?? tryWs();
  await store.load();
  if (client) {
    offStatus = client.onStatus((s) => {
      wsStatus.value = s as typeof wsStatus.value;
    });
  }
  await syncActive();
  // 布局变化不一定触发 xterm 的 onResize（侧栏动画、软键盘收起、浏览器
  // 缩放），所以留一个便宜的兜底：只在当前会话上量一次，尺寸没变就什么都不发。
  tick = setInterval(() => runtimes.get(store.activeId)?.checkSize(), 250);
});

onUnmounted(() => {
  if (tick) clearInterval(tick);
  tick = null;
  offStatus?.();
  offStatus = null;
  runtimes.disposeAll();
  hosts.clear();
});
</script>

<template>
  <div class="view term" :class="{ fs: fullscreen }">
    <div v-if="store.unavailable" class="tip err">
      <AppIcon name="alert" :size="16" />
      <span>终端不可用：{{ store.error || '服务端未启用终端模块（需要 tmux）' }}</span>
    </div>

    <template v-else>
      <div class="bar">
        <div class="tabs" role="tablist" aria-label="终端会话">
          <div
            v-for="s in store.sessions"
            :key="s.id"
            class="tab"
            :class="{ on: s.id === store.activeId, dead: !s.alive }"
            role="tab"
            :aria-selected="s.id === store.activeId"
            :title="s.alive ? s.tmux_name : '会话已退出：点击查看当前状态'"
            @click="pick(s.id)"
          >
            <span class="dot" />
            <span class="tname">{{ s.title }}</span>
            <span
              v-if="s.id === store.activeId"
              class="x"
              role="button"
              aria-label="关闭会话"
              @click.stop="confirmDelete = { id: s.id, title: s.title }"
              ><AppIcon name="close" :size="14"
            /></span>
            <span
              class="pen"
              role="button"
              aria-label="改名"
              @click.stop="renameTo = { id: s.id, title: s.title }"
              >✎</span
            >
          </div>
          <div
            class="tab new"
            role="button"
            aria-label="新建会话"
            @click="formOpen = true"
          >
            <AppIcon name="add" :size="15" />
          </div>
        </div>
        <div class="acts">
          <span v-if="offline" class="ws" :class="wsStatus">
            {{ wsStatus === 'connecting' ? '连接中…' : '已断开，正在重连' }}
          </span>
          <span v-if="dropped" class="ws warn">已丢弃 {{ dropped }} 次输入</span>
          <button class="ghost" :disabled="busy || store.unavailable" @click="store.reload()">
            <AppIcon name="info" :size="15" />刷新
          </button>
          <button v-if="isPhone" class="ghost" @click="fullscreen = !fullscreen">
            {{ fullscreen ? '退出全屏' : '全屏' }}
          </button>
        </div>
      </div>

      <div v-if="renameTo" class="inline">
        <input
          v-model="renameTo.title"
          class="in"
          placeholder="会话名称"
          @keyup.enter="doRename"
          @keyup.esc="renameTo = null"
        />
        <button class="ghost" @click="doRename">确定</button>
        <button class="ghost" @click="renameTo = null">取消</button>
      </div>

      <div class="stage" :class="{ empty: !activeRow }">
        <div
          v-for="s in store.sessions"
          :key="s.id"
          :ref="(el) => setPane(s.id, el)"
          class="pane"
          :class="{ hidden: s.id !== store.activeId }"
          role="tabpanel"
        />
        <div v-if="!store.sessions.length" class="blank">
          <p>{{ store.loading ? '载入中…' : '还没有终端会话' }}</p>
          <button class="go" :disabled="busy" @click="formOpen = true">新建会话</button>
        </div>
        <div v-else-if="activeRow && !activeRow.alive" class="blank">
          <p>会话「{{ activeRow.title }}」已退出。</p>
          <button class="go" :disabled="busy" @click="createSession">
            用同样的名字再开一个
          </button>
        </div>
      </div>
    </template>

    <Sheet v-if="formOpen" title="新建终端会话" icon="terminal" @close="formOpen = false">
      <label class="fld"
        >名称<input v-model="form.title" :placeholder="suggestion" /></label
      >
      <label class="fld"
        >起始目录<input v-model="form.cwd" placeholder="留空使用默认目录"
      /></label>
      <label class="fld"
        >历史行数
        <select v-model.number="form.history">
          <option v-for="n in HISTORY_LIMITS" :key="n" :value="n">
            {{ n === DEFAULT_HISTORY_LIMIT ? `${n}（默认）` : n }}
          </option>
        </select>
      </label>
      <p v-if="store.error" class="err">{{ store.error }}</p>
      <template #footer>
        <button class="ghost" @click="formOpen = false">取消</button>
        <button class="go" :disabled="store.creating" @click="createSession">创建</button>
      </template>
    </Sheet>

    <Sheet
      v-if="confirmDelete"
      title="关闭会话"
      icon="alert"
      @close="confirmDelete = null"
    >
      <p>
        会话「{{ confirmDelete.title }}」里正在运行的程序会被一起终止，且无法恢复。
      </p>
      <template #footer>
        <button class="ghost" @click="confirmDelete = null">取消</button>
        <button class="danger" @click="doDelete">确认关闭</button>
      </template>
    </Sheet>
  </div>
</template>

<style scoped>
.term {
  display: flex;
  flex-direction: column;
  gap: 8px;
  height: 100%;
  min-height: 0;
  padding: 10px;
}
.term.fs {
  position: fixed;
  inset: 0;
  z-index: 300;
  background: var(--bg);
  padding: 6px;
}
.bar {
  display: flex;
  align-items: center;
  gap: 8px;
  flex-wrap: wrap;
}
.tabs {
  display: flex;
  gap: 6px;
  overflow-x: auto;
  flex: 1;
  min-width: 0;
}
.tab {
  display: flex;
  align-items: center;
  gap: 5px;
  padding: 6px 9px;
  font-size: 12.5px;
  color: var(--text-dim);
  background: var(--card);
  border: 1px solid var(--border);
  border-radius: var(--radius-sm);
  white-space: nowrap;
  cursor: pointer;
}
.tab.on {
  color: var(--text);
  border-color: var(--accent);
}
.tab.dead {
  opacity: 0.55;
}
.tab.dead .dot {
  background: var(--text-mute);
}
.dot {
  width: 6px;
  height: 6px;
  border-radius: 50%;
  background: var(--ok);
}
.tname {
  max-width: 12em;
  overflow: hidden;
  text-overflow: ellipsis;
}
.x,
.pen {
  color: var(--text-mute);
  padding: 0 2px;
}
.x:hover {
  color: var(--err);
}
.pen:hover {
  color: var(--accent);
}
.tab.new {
  padding: 6px 10px;
  color: var(--accent);
}
.acts {
  display: flex;
  align-items: center;
  gap: 6px;
}
.ws {
  font-size: 11.5px;
  color: var(--text-mute);
}
.ws.connecting {
  color: var(--warn);
}
.ws.offline {
  color: var(--err);
}
.ws.warn {
  color: var(--warn);
}
.inline {
  display: flex;
  gap: 6px;
  align-items: center;
}
.in {
  flex: 1;
  min-width: 0;
}
.stage {
  position: relative;
  flex: 1;
  min-height: 260px;
  border: 1px solid var(--border);
  border-radius: var(--radius-sm);
  background: var(--card);
  overflow: hidden;
}
.pane {
  position: absolute;
  inset: 0;
  padding: 6px 4px 6px 8px;
}
/* v-show 而不是 v-if：v-if 会把容器一起摘掉，重建时 xterm 要重新
   open() 一次（重新量字体、重新申请 canvas），切换会明显卡一下。
   实例本身只保留当前会话那一个，见脚本顶部。 */
.pane.hidden {
  display: none;
}
.blank {
  position: absolute;
  inset: 0;
  display: flex;
  flex-direction: column;
  align-items: center;
  justify-content: center;
  gap: 10px;
  color: var(--text-dim);
  font-size: 13px;
}
.stage:not(.empty) .blank {
  /* 会话存在但当前这个已退出：半透明浮层，别把最后的输出全挡住 */
  background: color-mix(in srgb, var(--card) 88%, transparent);
}
.stage.empty .blank {
  background: var(--card);
}
.tip {
  display: flex;
  align-items: center;
  gap: 6px;
  padding: 10px;
  border-radius: var(--radius-sm);
  font-size: 12.5px;
}
.tip.err {
  color: var(--err);
  border: 1px solid var(--err);
}
.fld {
  display: flex;
  flex-direction: column;
  gap: 5px;
  font-size: 12.5px;
  color: var(--text-dim);
  margin-bottom: 10px;
}
.err {
  color: var(--err);
  font-size: 12.5px;
}
button {
  display: inline-flex;
  align-items: center;
  gap: 4px;
  font: inherit;
  font-size: 12.5px;
  border-radius: var(--radius-sm);
  padding: 6px 10px;
  cursor: pointer;
  border: 1px solid var(--border);
}
button:disabled {
  opacity: 0.5;
  cursor: default;
}
.ghost {
  background: transparent;
  color: var(--text-dim);
}
.go {
  background: var(--accent);
  border-color: var(--accent);
  color: #fff;
  font-weight: 600;
}
.danger {
  background: var(--err);
  border-color: var(--err);
  color: #fff;
  font-weight: 600;
}
</style>
