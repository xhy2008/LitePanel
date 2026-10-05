<script setup lang="ts">
import { computed, nextTick, onMounted, onUnmounted, ref, watch } from 'vue';
import { Terminal } from '@xterm/xterm';
import { FitAddon } from '@xterm/addon-fit';
import '@xterm/xterm/css/xterm.css';
import AppIcon from '../components/AppIcon.vue';
import Sheet from '../components/services/Sheet.vue';
import { useTerminalStore, autoTitle, deathBadge } from '../stores/terminal';
import { useQuickCmdStore, tabBadge } from '../stores/quickcmd';
import { useBusyWatch } from '../composables/useBusyWatch';
import { createTerminalRuntime } from '../composables/useTerminal';
import { useTermRuntimes, type Runtime } from '../composables/useTermRuntimes';
import { termChannel, HISTORY_LIMITS } from '../api/terminal';
import { useBreakpoint } from '../composables/useBreakpoint';
import { useRoute, useRouter } from 'vue-router';
import { tryWs, type WsClient } from '../api/ws';
import { cwdOfQuery, stripCwd } from '../composables/cwdQuery';

// 终端视图（设计 7.4）。同一时刻只保留**一个** xterm 实例：每个实例都带着
// 屏幕缓冲、一条 WS 订阅和一个尺寸轮询，留着"看过的全部"就是留着 N 份
// 输出在往浏览器灌，而用户只看得到一个。切回来的画面由服务端重放补齐
// （tmux 的 history-limit 才是历史的唯一归属，前端 scrollback 特意设 0）。
const props = defineProps<{ wsClient?: WsClient }>();

const store = useTerminalStore();
// 忙闲存在 quickcmd store 上（那里是 /api/commands/busy 的唯一客户端）：
// 两个页面各打一枪就是双倍往返，而且两份结论会不一致 —— 用户在命令页
// 看到"空闲"、终端标签上却写着"忙"。
const cmds = useQuickCmdStore();
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
const route = useRoute();
const router = useRouter();
const fullscreen = ref(false);
const busy = computed(() => store.loading || store.creating);

// 抽屉
const formOpen = ref(false);
const form = ref<{ title: string; cwd: string; history: number | null }>({
  title: '',
  cwd: '',
  // null = 跟随面板默认（走设置页的 term_history_limit）。这里**故意**不硬编码
  // 一个数字：只要表单总显式发送 history_limit，设置页上那个默认值就永远
  // 读不到，成了只存不读的摆设。
  history: null,
});
// dead：从尸体标签进来的删除。确认文案必须区分 —— 对着一具尸体说
// "正在运行的程序会被一起终止"是错的，这时唯一被删掉的是遗言记录。
const confirmDelete = ref<{ id: number; title: string; dead?: boolean } | null>(null);
const renameTo = ref<{ id: number; title: string } | null>(null);

let client: WsClient | null = null;
let tick: ReturnType<typeof setInterval> | null = null;
// 忙闲轮询。"命令结束了"这件事不会推给任何人，不轮询就只能靠用户点标签
// 去碰 —— 于是快捷命令跑完之后角标永久停在"忙"，下一次点命令就白开一个
// 会话（实际踩到的就是这个）。
// 每一轮先重拉会话列表、再查忙闲：用户在 tmux 里 exit 之后标签要自己
// 消失，而"会话退出了"同样没有任何推送。
const busyWatch = useBusyWatch(cmds, undefined, () => store.reload());
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
// 当前标签是尸体（异常退出/消失）：不挂 xterm（尸体上 attach 出来的
// 控制连接是半开的假连接），显示遗言浮层。
const deadActive = computed(() => !!activeRow.value && !activeRow.value.alive);
const deadBadge = computed(() => (activeRow.value ? deathBadge(activeRow.value) : ''));
// 遗言是为哪个会话拉的。不能拿 output==='' 当"还没拉过"：会话消失时
// 遗言本来就是空串，那样判会每拍重拉；而在 watcher 里清 output 再拉
// 则是自激循环（清空 -> deadActive 分支再进 watcher）。
const outputFor = ref(0);
const offline = computed(() => wsStatus.value !== 'online');
const suggestion = computed(() => autoTitle(form.value.title, store.sessions));

// 当前会话换了，就要有实例 —— 除非它已经是一具尸体。
async function syncActive() {
  await nextTick();
  // 尸体不挂 xterm：attach 到尸体上，控制连接会半开地挂着（tmux 对
  // remain-on-exit 的会话照常回 attach 成功），屏幕从此不再有任何输出。
  // active 是尸体时遗言浮层接管这个 pane 的位置。
  if (deadActive.value) {
    runtimes.focus(0); // 回收现有实例：留着它只会继续收上一个会话的输出
    return;
  }
  runtimes.focus(activeId.value || 0);
}

watch(activeId, syncActive);

// 正在看的会话当场死掉：activeId 不变，上面的 watch 不会响。
watch(deadActive, (dead) => {
  if (dead && activeRow.value && outputFor.value !== activeRow.value.id) {
    outputFor.value = activeRow.value.id;
    store.loadOutput(activeRow.value.id);
  }
});

async function pick(id: number) {
  const row = store.sessions.find((x) => x.id === id);
  if (row && !row.alive) {
    // 尸体标签可以点：看的是死前最后的输出（遗言），不是终端。
    // 先拉遗言、后切 activeId：loadOutput 同步清空 + 异步填充，如果先切
    // 过去，中间那一帧浮层显示的是空/上一个会话的内容。
    const prev = store.activeId;
    outputFor.value = id;
    await store.loadOutput(id);
    // 等待期间用户又点了别的标签（active 已被改动）：这次切换作废，
    // 遗言已经进 store 也无所谓 —— active 不是它，浮层不会渲染它。
    if (store.activeId !== prev) return;
    store.select(id);
    await syncActive();
    return;
  }
  if (id === store.activeId) return;
  store.select(id);
  await syncActive();
}

// 读完就把 query 抹掉：留着的话，用户之后刷新页面会再弹一次抽屉,
// 而那次他并没有"从文件页跳转"的意图。replace 而不是 push：这次改写
// 不该在历史里占一格，否则"后退"会退回一个仍然带 ?cwd= 的地址,
// 再进去又弹一次。
function applyCwdQuery() {
  const cwd = cwdOfQuery(route.query);
  if (!cwd) return;
  form.value = { ...form.value, cwd };
  formOpen.value = true;
  void router.replace({ name: 'term', query: stripCwd(route.query) });
}

async function createSession() {
  try {
    await store.create({
      title: form.value.title,
      cwd: form.value.cwd.trim() || undefined,
      history_limit: form.value.history ?? undefined,
    });
    formOpen.value = false;
    form.value = { title: '', cwd: '', history: null };
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
  // 忙闲角标（原型 lp-2 · 忙）：start 立刻查一次，之后按固定节拍续查。
  // 之前这里是"切标签/建会话时各查一次"散在三处 —— 那只是"没人轮询"
  // 的补丁，而漏掉的那一处就是角标卡在忙不动。
  busyWatch.start();
  // 从文件页"在终端中打开"跳过来时带着 ?cwd=：替用户把新建会话的抽屉
  // 打开并填好目录，而不是让他再手打一遍绝对路径。
  //
  // 打开抽屉而不是**直接建会话**：一个目录一个会话地跳转会在一小时内
  // 攒出几十个同名标签，而用户没要求过这些。
  applyCwdQuery();
  // 布局变化不一定触发 xterm 的 onResize（侧栏动画、软键盘收起、浏览器
  // 缩放），所以留一个便宜的兜底：只在当前会话上量一次，尺寸没变就什么都不发。
  tick = setInterval(() => runtimes.get(store.activeId)?.checkSize(), 250);
});

onUnmounted(() => {
  busyWatch.stop();
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
            :class="{ on: s.id === store.activeId }"
            role="tab"
            :aria-selected="s.id === store.activeId"
            :title="s.alive ? s.tmux_name : deathBadge(s) + '：点击查看最后的输出'"
            @click="pick(s.id)"
          >
            <span class="dot" :class="{ dead: !s.alive }" />
            <span class="tname">{{ s.title }}</span>
            <span v-if="!s.alive" class="tdead">{{ deathBadge(s) }}</span>
            <span v-if="tabBadge(cmds.busy[s.id])" class="tbusy">{{ tabBadge(cmds.busy[s.id]) }}</span>
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
        <!-- 尸体标签被点进来：浮层顶替终端。不挂 xterm —— attach 到尸体上
             控制连接会半开地挂着（tmux 对 remain-on-exit 照常回成功），
             屏幕从此不再有任何输出，比不显示更难解释。
             遗言：capture-pane 在尸体上读得到 grid；空串=没有可读历史
             （会话整个消失过，grid 随 server 没了）。 -->
        <div v-else-if="deadActive" class="blank">
          <p>
            会话「{{ activeRow?.title }}」{{
              activeRow?.exit_status === -1 ? '已消失（读不到退出码）' : `命令异常退出（退出码 ${activeRow?.exit_status ?? '?'}）`
            }}
          </p>
          <pre v-if="store.output" class="lastwords">{{ store.output }}</pre>
          <p v-else class="lastwords-hint">无输出记录（会话整个消失，历史随 tmux 服务一起没了）</p>
          <div class="row">
            <button class="ghost" :disabled="busy" @click="confirmDelete = { id: activeRow!.id, title: activeRow!.title, dead: true }">删除</button>
            <button class="go" :disabled="busy" @click="formOpen = true">新建会话</button>
          </div>
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
        <select v-model="form.history">
          <option :value="null">跟随面板默认</option>
          <option v-for="n in HISTORY_LIMITS" :key="n" :value="n">{{ n }}</option>
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
      <p v-if="confirmDelete.dead">
        会话「{{ confirmDelete.title }}」已经退出。删除后这段最后的输出也会一并清除，且无法恢复。
      </p>
      <p v-else>
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
.dot {
  width: 6px;
  height: 6px;
  border-radius: 50%;
  background: var(--ok);
}
/* 死因角标：红底小号，与"忙"（tbusy，黄色）用同一套位置但颜色分流 ——
   用户扫一眼标签栏就该分出"在跑 / 空闲 / 出事了"。 */
.tdead {
  font-size: 10.5px;
  color: var(--err);
  border: 1px solid var(--err);
  border-radius: 999px;
  padding: 0 6px;
  white-space: nowrap;
}
.dot.dead {
  background: var(--err);
}
.lastwords {
  width: min(720px, 92%);
  max-height: 46vh;
  overflow: auto;
  margin: 0;
  padding: 10px;
  text-align: left;
  font-size: 12px;
  line-height: 1.5;
  white-space: pre-wrap;
  word-break: break-all;
  background: color-mix(in srgb, var(--card) 60%, transparent);
  border: 1px solid var(--border);
  border-radius: var(--radius-sm);
  color: var(--text);
}
.lastwords-hint {
  color: var(--text-dim);
}
.row {
  display: flex;
  gap: 8px;
}
.tbusy {
  font-size: 10px;
  color: var(--warn);
  flex: none;
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
