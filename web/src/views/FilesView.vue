<script setup lang="ts">
import { computed, onMounted, onUnmounted, ref, watch } from 'vue';
import { useRoute, useRouter } from 'vue-router';
import AppIcon from '../components/AppIcon.vue';
import Sheet from '../components/services/Sheet.vue';
import FileRow from '../components/files/FileRow.vue';
import Breadcrumb from '../components/files/Breadcrumb.vue';
import PathInput from '../components/files/PathInput.vue';
import ContextMenu from '../components/files/ContextMenu.vue';
import UploadDropZone from '../components/files/UploadDropZone.vue';
import TrashPanel from '../components/files/TrashPanel.vue';
import { menuItemsFor } from '../components/files/fsMenu';
import { justFinished, touchesDir } from '../components/files/jobDirs';
import type { MenuItem } from '../components/files/ContextMenu.vue';
import { useFilesStore } from '../stores/files';
import { useUploadsStore } from '../stores/uploads';
import { useFsJobsStore } from '../stores/fsJobs';
import { downloadUrl, zipUrl, joinPath } from '../api/files';
import type { FsEntry, SortKey } from '../api/files';
import type { ConflictPolicy } from '../api/upload';
import { sizeLabel } from '../api/files';

// 文件管理主视图（设计 8.1–8.6 的汇合点）。
//
// 这一层刻意做薄：列表分页、选择、剪贴板、回收站的业务都在 stores/files,
// 上传协议在即 stores/uploads,菜单项规则在 fsMenu。视图只做三件事：
// 把用户手势翻译成 store 调用、把 store 状态渲染出来、以及处理只有
// "同时看着屏幕"才知道的交互（滚动分页、菜单定位、拖放）。
const store = useFilesStore();
const uploads = useUploadsStore();
const jobs = useFsJobsStore();
const route = useRoute();
const router = useRouter();

onMounted(() => {
  // query ?dir= 是为了"从别处跳进来落在某个目录"（终端里 cd 过来、
  // 下载页回看来源等）。没有它就走 store 上一次的状态。
  const q = typeof route.query.dir === 'string' ? route.query.dir : '';
  void store.open(q || store.dir || '/');
  void store.loadRoots();
});

// ---- 列表与分页 ----

const rootPaths = computed(() => store.roots.map((r) => r.path));

function onRowOpen(e: FsEntry) {
  if (e.is_dir) {
    void store.open(joinPath(store.dir, e.name));
    return;
  }
  // 单击文件即选中（手机端的设计：没有"打开方式"这个中间态）。
  store.toggleSelect(e.name);
}

// 滚动分页：到底部附近就追下一页。用滚动位置而不是 IntersectionObserver
// 的哨兵元素：哨兵在虚拟/长列表里会因为高度未定而提前触发。
const scroller = ref<HTMLElement | null>(null);
function onScroll() {
  const el = scroller.value;
  if (!el || store.loading || store.loadingMore || !store.hasMore) return;
  if (el.scrollTop + el.clientHeight >= el.scrollHeight - 300) void store.loadMore();
}

// ---- 排序 / 显示隐藏 ----

const SORTS: Array<{ k: SortKey; label: string }> = [
  { k: 'name', label: '名称' },
  { k: 'size', label: '大小' },
  { k: 'mtime', label: '时间' },
  { k: 'type', label: '类型' },
];
const sortLabel = computed(() => SORTS.find((s) => s.k === store.sort)?.label ?? '名称');

// ---- 菜单 ----

const menu = ref<{ items: MenuItem[]; x: number; y: number } | null>(null);
const menuTarget = ref<FsEntry | null>(null);

function openMenu(e: FsEntry | null, x: number, y: number) {
  // 右键已选中项时不缩小选择（多选后右键第二个的预期是"对这多个操作"）,
  // 右键未选中项时才把选择改成那一项。
  let selectedCount = store.selected.length;
  if (e && !store.selected.includes(e.name)) {
    store.clearSelection();
    store.toggleSelect(e.name);
    selectedCount = 1;
    menuTarget.value = e;
  } else {
    menuTarget.value = e;
  }
  menu.value = {
    items: menuItemsFor({
      entry: e ? { name: e.name, is_dir: e.is_dir } : null,
      selectedCount,
      hasClip: store.canPaste,
      multi: selectedCount > 1,
      canOpenTerminal: true,
    }),
    x,
    y,
  };
}

function onRowMenu(e: FsEntry, x: number, y: number) {
  openMenu(e, x, y);
}

function onBgMenu(ev: MouseEvent) {
  // 点在行上时由行自己处理（行里已经 preventDefault 过）。
  openMenu(null, ev.clientX, ev.clientY);
}

async function onPick(key: string) {
  const t = menuTarget.value;
  const single = t ? [joinPath(store.dir, t.name)] : [];
  const targets = store.selectedPaths.length ? store.selectedPaths : single;
  menu.value = null;
  switch (key) {
    case 'open':
      if (t?.is_dir) void store.open(joinPath(store.dir, t.name));
      break;
    case 'download':
      download(targets);
      break;
    case 'mkdir':
      mkdirOpen.value = true;
      mkdirName.value = '';
      break;
    case 'rename':
      if (t) {
        renameTo.value = { from: t.name, to: t.name };
      }
      break;
    case 'cut':
      store.setClip('cut');
      break;
    case 'copy':
      store.setClip('copy');
      break;
    case 'paste':
      await doPaste();
      break;
    case 'refresh':
      void store.open(store.dir);
      break;
    case 'delete':
      askDelete();
      break;
    case 'terminal':
      await openTerminal();
      break;
    case 'cpath':
      await copyPaths(targets);
      break;
    case 'props':
      propsTarget.value = t ? targets : [store.dir];
      break;
    default:
      break;
  }
}

// ---- 下载 ----

function download(paths: string[]) {
  if (!paths.length) return;
  // 目录一律走 zip：后端把目录当目录流（design 8.3），而单个文件的
  // 直链对目录必然失败。
  const one = paths.length === 1 ? store.entries.find((e) => joinPath(store.dir, e.name) === paths[0]) : null;
  const url = one && !one.is_dir ? downloadUrl(paths[0]) : zipUrl(paths);
  const a = document.createElement('a');
  a.href = url;
  // 多文件时让浏览器自己起 zip 的名字（后端在 Content-Disposition 里给了
  // 准确的名字与 UTF-8 编码；在这里猜等于猜错一次就多一个坏文件名）。
  document.body.appendChild(a);
  a.click();
  a.remove();
}

// ---- 剪贴板 ----

const clipLabel = computed(() => {
  if (!store.clip) return '';
  return `${store.clip.mode === 'cut' ? '剪切' : '复制'} ${store.clip.paths.length} 项 → ${store.dir}`;
});

async function doPaste() {
  const id = await store.paste();
  if (id) jobsOpenHint();
}

// 提交完任务之后不去开抽屉、只给一条提示（抽屉是全局面板，用户自己会看）,
// 但必须提示任务号：那是唯一能在抽屉里认出这条任务的凭据。
const hint = ref('');
let hintTimer: ReturnType<typeof setTimeout> | null = null;
function showHint(msg: string) {
  hint.value = msg;
  if (hintTimer) clearTimeout(hintTimer);
  // 必须自己消失：常驻的提示会被读成"当前状态"，而它说的其实是
  // "刚刚发生过一件事"。
  hintTimer = setTimeout(() => (hint.value = ''), 3000);
}
function jobsOpenHint() {
  showHint('任务已提交，右下角任务的进度可点开查看');
}

// ---- 选择栏 ----

const selectionLabel = computed(() => `${store.selected.length} 项`);

function cutPathsOf(paths: string[]): boolean {
  return !!store.clip && store.clip.mode === 'cut' && store.clip.paths.some((p) => paths.includes(p));
}

// ---- 新建 / 重命名 / 删除确认 ----

const mkdirOpen = ref(false);
const mkdirName = ref('');
const mkdirErr = ref('');
const renameTo = ref<{ from: string; to: string } | null>(null);
const renameErr = ref('');
const confirmDel = ref(false);
const permanentDel = ref(false);
const propsTarget = ref<string[] | null>(null);

async function submitMkdir() {
  mkdirErr.value = '';
  try {
    await store.mkdir(mkdirName.value.trim());
    mkdirOpen.value = false;
  } catch (e) {
    mkdirErr.value = (e as Error).message || '创建失败';
  }
}

async function submitRename() {
  if (!renameTo.value) return;
  renameErr.value = '';
  try {
    await store.rename(renameTo.value.from, renameTo.value.to.trim());
    renameTo.value = null;
  } catch (e) {
    renameErr.value = (e as Error).message || '重命名失败';
  }
}

// 删除的默认永远是"进回收站"。永久删除这个选项只在两种情况下出现:
// 用户明确要求，或这个盘的回收站根本建不起来（只读盘）。
function askDelete() {
  confirmDel.value = true;
  permanentDel.value = store.trashUnwritable;
}

async function confirmDeleteNow() {
  try {
    const id = await store.deleteSelected(permanentDel.value);
    confirmDel.value = false;
    if (id) jobsOpenHint();
  } catch {
    /* trashUnwritable 已被 store 置位，确认框里会多出一个选项 */
  }
}

// ---- 回收站 ----

const trashOpen = ref(false);

// ---- 任务完成后把列表刷回来 ----
//
// 删除/粘贴提交的是后台队列任务。提交完如果就此"失聪",用户删完看到的
// 还是原来那一列（文件明明已从盘上没了），第一反应是没删掉、会再删一次。
// 队列的闭环 = 任务到终态时把受影响的目录刷回来。
//
// 只在用户**正看着**那个目录时刷（touchesDir 为真且 dir 没变）:
// 无脑刷新会把他正在翻的目录弹回第一页。跟 stores/uploads 的 refresh 同
// 一条规则。
//
// 为什么 watch items 而不是订阅 WS done 事件：JobDrawer 已经在消费 fsjobs
// 推送并写进同一个 store，这里读 store 的快照变化即可，不必重复接一路 WS。
let prevActive = new Map<number, boolean>();
watch(
  () => jobs.items,
  (items) => {
    const dir = store.dir;
    const next = new Map<number, boolean>();
    for (const j of items) {
      const active = j.state === 'pending' || j.state === 'running';
      next.set(j.id, active);
      const was = prevActive.get(j.id);
      if (active) continue;
      // 上一帧活跃、这一帧终态，且改的就是正在看的目录 —— 刷。
      if (was === true && justFinished(was, j) && touchesDir(j, dir)) {
        void store.open(dir);
      }
    }
    prevActive = next;
  },
  { deep: true },
);

// ---- 上传 ----

const fileInput = ref<HTMLInputElement | null>(null);
function pickFiles() {
  fileInput.value?.click();
}
function onPicked(e: Event) {
  const el = e.target as HTMLInputElement;
  const files = el.files ? Array.from(el.files) : [];
  el.value = ''; // 允许连点两次选同一个文件（不重置就不会触发 change）
  if (!files.length) return;
  uploads.enqueue(
    files.map((f) => ({
      name: f.name,
      size: f.size,
      mtime: f.lastModified ? Math.round(f.lastModified / 1000) : 0,
      slice: (s: number, en: number) => f.slice(s, en),
    })),
  );
}

const activeUploads = computed(() => uploads.items.filter((i) => i.status !== 'done' && i.status !== 'canceled'));

const conflict = computed(() => uploads.items.find((i) => i.status === 'conflict') ?? null);
function resolveConflict(p: ConflictPolicy) {
  const it = conflict.value;
  if (it) uploads.resolve(it.id, p);
}

// ---- 终端 ----

async function openTerminal() {
  const dir = menuTarget.value?.is_dir
    ? joinPath(store.dir, menuTarget.value.name)
    : store.dir;
  // 用 query 而不是 store.notice 传话：notice 会被终端页的忙闲轮询每几秒
  // 清空一次（它把"没人死亡的那一轮"当作"没有消息"），跳过去之后可能
  // 在用户读到之前就没了。
  await router.push({ name: 'term', query: { cwd: dir } });
}

// ---- 复制到剪贴板 ----

async function copyPaths(paths: string[]) {
  const text = paths.join('\n');
  try {
    await navigator.clipboard?.writeText(text);
    showHint('路径已复制');
  } catch {
    // 非安全上下文（http://内网 IP）里剪贴板 API 直接抛：把内容显示出来,
    // 至少用户能自己选中复制，不至于什么都不发生。
    showHint(text);
  }
}

onUnmounted(() => {
  if (hintTimer) clearTimeout(hintTimer);
});

</script>

<template>
  <div class="view files">
    <div class="bar">
      <Breadcrumb :path="store.dir" @go="(p) => store.open(p)" />
      <PathInput :model-value="store.dir" @submit="(p) => store.open(p)" />
      <div class="tools">
        <button class="tb" @click="mkdirOpen = true"><AppIcon name="add" :size="15" />新建目录</button>
        <button class="tb" :disabled="!store.canPaste" @click="doPaste"><AppIcon name="paste" :size="15" />粘贴</button>
        <button class="tb" @click="pickFiles"><AppIcon name="upload" :size="15" />上传</button>
        <button class="tb" @click="trashOpen = true"><AppIcon name="trash" :size="15" />回收站</button>
      </div>
      <div class="tools2">
        <select class="sb" :value="store.sort" @change="store.setSort(($event.target as HTMLSelectElement).value as SortKey)">
          <option v-for="s in SORTS" :key="s.k" :value="s.k">{{ s.label }}</option>
        </select>
        <button class="tb" :class="{ on: store.desc }" @click="store.setSort(store.sort)">
          {{ store.desc ? '降序' : '升序' }}
        </button>
        <button class="tb" :class="{ on: store.hidden }" @click="store.toggleHidden">隐藏文件</button>
      </div>
    </div>

    <div v-if="rootPaths.length" class="roots">
      <button v-for="r in rootPaths" :key="r" class="rb" :class="{ on: store.dir === r }" @click="store.open(r)">
        {{ r }}
      </button>
    </div>

    <div v-if="store.error" class="err">{{ store.error }}</div>
    <div v-if="hint" class="hint">{{ hint }}</div>
    <div v-if="store.clip" class="clip">{{ clipLabel }}</div>

    <div class="up" v-if="activeUploads.length">
      <div v-for="u in activeUploads" :key="u.id" class="upr">
        <span class="un">{{ u.name }}</span>
        <div class="ub"><div class="ubf" :style="{ width: (u.size ? Math.round(u.received / u.size * 100) : 0) + '%' }" /></div>
        <span class="us">{{ sizeLabel(u.received) }}</span>
        <button class="xc" @click="uploads.cancel(u.id)">取消</button>
      </div>
    </div>

    <div
      ref="scroller"
      class="list"
      @scroll="onScroll"
      @contextmenu.prevent="onBgMenu"
    >
      <UploadDropZone :dir="store.dir">
        <div v-if="store.loading" class="muted">加载中…</div>
        <div v-else-if="!store.entries.length" class="muted">空目录</div>
        <template v-else>
          <FileRow
            v-for="e in store.entries"
            :key="e.name"
            :entry="e"
            :selected="store.selected.includes(e.name)"
            :cut="cutPathsOf([joinPath(store.dir, e.name)])"
            @open="onRowOpen"
            @toggle="store.toggleSelect(e.name)"
            @menu="onRowMenu"
          />
        </template>
        <div v-if="store.loadingMore" class="muted">加载中…</div>
      </UploadDropZone>
    </div>

    <div v-if="store.selected.length" class="selbar">
      <span>{{ selectionLabel }}</span>
      <button class="tb" @click="store.selectAll()">{{ store.allSelected ? '取消全选' : '全选' }}</button>
      <button class="tb" @click="store.setClip('cut')">剪切</button>
      <button class="tb" @click="store.setClip('copy')">复制</button>
      <button class="tb d" @click="askDelete">删除</button>
      <button class="tb" @click="store.clearSelection()">取消</button>
    </div>

    <ContextMenu v-if="menu" :items="menu.items" :x="menu.x" :y="menu.y" @pick="onPick" @close="menu = null" />

    <input ref="fileInput" type="file" multiple hidden @change="onPicked" />

    <Sheet v-if="mkdirOpen" title="新建目录" icon="add" @close="mkdirOpen = false">
      <input v-model="mkdirName" class="fi" placeholder="目录名" @keydown.enter="submitMkdir" />
      <p v-if="mkdirErr" class="er">{{ mkdirErr }}</p>
      <template #footer>
        <button class="tb" @click="mkdirOpen = false">取消</button>
        <button class="tb go" :disabled="!mkdirName.trim()" @click="submitMkdir">创建</button>
      </template>
    </Sheet>

    <Sheet v-if="renameTo" title="重命名" icon="edit" @close="renameTo = null">
      <input v-model="renameTo.to" class="fi" @keydown.enter="submitRename" />
      <p v-if="renameErr" class="er">{{ renameErr }}</p>
      <template #footer>
        <button class="tb" @click="renameTo = null">取消</button>
        <button class="tb go" :disabled="!renameTo.to.trim()" @click="submitRename">确定</button>
      </template>
    </Sheet>

    <Sheet v-if="confirmDel" title="删除" icon="trash" @close="confirmDel = false">
      <p class="q">把 {{ store.selected.length }} 项移入回收站？回收站里的文件保留 3 天。</p>
      <p v-if="store.trashUnwritable" class="q w">
        这个目录所在的盘建不了回收站（只读或不可写），只能永久删除。
      </p>
      <label class="pm"><input v-model="permanentDel" type="checkbox" />不进回收站，直接永久删除（不可恢复）</label>
      <template #footer>
        <button class="tb" @click="confirmDel = false">取消</button>
        <button class="tb d go" @click="confirmDeleteNow">{{ permanentDel ? '确认永久删除' : '移入回收站' }}</button>
      </template>
    </Sheet>

    <Sheet v-if="conflict" title="目标已存在同名文件" icon="properties" @close="uploads.cancel(conflict.id)">
      <p class="q">{{ conflict.name }} 已存在于 {{ conflict.dir }}。</p>
      <template #footer>
        <button class="tb" @click="resolveConflict('skip')">跳过</button>
        <button class="tb" @click="resolveConflict('rename')">换个名字</button>
        <button class="tb d" @click="resolveConflict('overwrite')">覆盖</button>
      </template>
    </Sheet>

    <Sheet v-if="propsTarget" title="属性" icon="properties" @close="propsTarget = null">
      <div v-for="p in propsTarget" :key="p" class="pp">{{ p }}</div>
      <template #footer>
        <button class="tb" @click="propsTarget = null">关闭</button>
      </template>
    </Sheet>

    <TrashPanel v-if="trashOpen" @close="trashOpen = false" />
  </div>
</template>

<style scoped>
.files {
  display: flex;
  flex-direction: column;
  height: 100%;
  min-height: 0;
}
.bar {
  display: flex;
  align-items: center;
  gap: 8px;
  flex-wrap: wrap;
  padding: 8px 10px;
  border-bottom: 1px solid var(--border);
}
.tools,
.tools2 {
  display: flex;
  gap: 4px;
  flex-shrink: 0;
}
.tb {
  display: inline-flex;
  align-items: center;
  gap: 4px;
  background: transparent;
  border: 1px solid var(--border);
  border-radius: 5px;
  color: var(--text-dim);
  font-size: 11px;
  padding: 5px 8px;
  cursor: pointer;
  white-space: nowrap;
}
.tb.on {
  color: var(--accent);
  border-color: var(--accent);
}
.tb:disabled {
  opacity: 0.45;
  cursor: default;
}
.tb.d {
  color: var(--err);
}
.tb.go {
  background: var(--accent);
  border-color: var(--accent);
  color: #fff;
}
.sb {
  background: var(--bg);
  color: var(--text-dim);
  border: 1px solid var(--border);
  border-radius: 5px;
  font-size: 11px;
  padding: 5px 6px;
}
.roots {
  display: flex;
  gap: 4px;
  padding: 6px 10px;
  overflow-x: auto;
  border-bottom: 1px solid var(--border);
}
.rb {
  background: transparent;
  border: 1px solid var(--border);
  border-radius: 12px;
  color: var(--text-dim);
  font-size: 11px;
  padding: 3px 10px;
  cursor: pointer;
  white-space: nowrap;
}
.rb.on {
  border-color: var(--accent);
  color: var(--accent);
}
.err {
  padding: 6px 10px;
  font-size: 12px;
  color: var(--err);
}
.hint {
  padding: 6px 10px;
  font-size: 12px;
  color: var(--accent);
}
.clip {
  padding: 5px 10px;
  font-size: 11px;
  color: var(--text-mute);
  background: var(--bg-elev);
}
.up {
  padding: 4px 10px;
  border-bottom: 1px solid var(--border);
}
.upr {
  display: flex;
  align-items: center;
  gap: 8px;
  padding: 3px 0;
  font-size: 11px;
}
.un {
  width: 120px;
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
  color: var(--text-dim);
}
.ub {
  flex: 1;
  height: 4px;
  background: var(--card-2);
  border-radius: 2px;
  overflow: hidden;
}
.ubf {
  height: 100%;
  background: var(--accent);
}
.us {
  width: 64px;
  text-align: right;
  color: var(--text-mute);
  font-variant-numeric: tabular-nums;
}
.xc {
  background: transparent;
  border: 0;
  color: var(--text-mute);
  font-size: 11px;
  cursor: pointer;
}
.list {
  flex: 1;
  min-height: 0;
  overflow-y: auto;
  padding: 4px 6px;
}
.muted {
  padding: 20px;
  text-align: center;
  font-size: 12px;
  color: var(--text-mute);
}
.selbar {
  display: flex;
  align-items: center;
  gap: 6px;
  padding: 8px 10px;
  border-top: 1px solid var(--border);
  background: var(--bg-elev);
  font-size: 12px;
  color: var(--text-dim);
}
.fi {
  width: 100%;
  background: var(--bg);
  border: 1px solid var(--border);
  border-radius: 5px;
  color: var(--text);
  font-size: 13px;
  padding: 7px 8px;
}
.er {
  font-size: 12px;
  color: var(--err);
  margin-top: 6px;
}
.q {
  font-size: 12px;
  color: var(--text-dim);
  line-height: 1.5;
}
.q.w {
  color: var(--warn);
}
.pm {
  display: flex;
  align-items: center;
  gap: 6px;
  font-size: 12px;
  color: var(--text-dim);
  margin-top: 10px;
}
.pp {
  font-size: 12px;
  font-family: ui-monospace, monospace;
  color: var(--text-dim);
  padding: 3px 0;
  word-break: break-all;
}
</style>
