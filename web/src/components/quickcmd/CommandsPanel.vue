<script setup lang="ts">
import { onMounted, onUnmounted, ref, watch } from 'vue';
import { useRouter } from 'vue-router';
import CommandTile from './CommandTile.vue';
import AddCommandDialog from './AddCommandDialog.vue';
import Sheet from '../services/Sheet.vue';
import AppIcon from '../AppIcon.vue';
import { useQuickCmdStore } from '../../stores/quickcmd';
import { useTerminalStore } from '../../stores/terminal';
import type { CommandRow } from '../../api/quickcmd';

// 快捷命令列（设计 16.3 右列）。点击即执行（D20）：命令注入 tmux 会话，
// 页面切到终端页并选中那个标签 —— 命令输出的唯一去处就是终端，页面底部
// 不再有输出面板。
const store = useQuickCmdStore();
const term = useTerminalStore();
const router = useRouter();

const formFor = ref<number | 'new' | null>(null);
// 待执行确认的命令。危险命令的判定来自后端（need_confirm），这里只负责
// 在"注入之前"把用户拦一下：先注入再弹确认框，确认框就只是个通知。
const pendingRun = ref<CommandRow | null>(null);
const pendingDelete = ref<CommandRow | null>(null);

// toast 的文本。结果本身挂在 store.notice 上：磁贴点完就跳页，这个组件
// 会被卸载，return 的值没有任何人会读，只能由下一个接手的人取走。
// toast 必须自己消失：常驻的提示会被读成"当前状态"，而它说的其实是
// "刚才新建过一个会话"。
const toastMsg = ref('');
let toastTimer: ReturnType<typeof setTimeout> | null = null;

function showToast(msg: string) {
  if (!msg) return;
  toastMsg.value = msg;
  if (toastTimer) clearTimeout(toastTimer);
  toastTimer = setTimeout(() => (toastMsg.value = ''), 2600);
}

onMounted(async () => {
  drainNotice();
  await store.load();
  // 会话列表可能还没加载过（用户直接进命令页）。忙闲是会话的属性，
  // 没有会话清单就无从问起。
  if (!term.loaded) await term.load();
  await store.refreshBusy();
});

onUnmounted(() => {
  if (toastTimer) clearTimeout(toastTimer);
  store.clearNotice();
});

// notice 是提示的唯一来源，显示后立刻清空。清空不是为了好看：
// notice 的值不变就不会再触发 watch，于是"这条命令又去了一个新会话"
// 这类每次都一样的文案，第二次点就没有任何提示了 —— 而用户正因为
// 看不见才连点。挂载时也取一次：本次执行可能是在别的页触发的。
function drainNotice() {
  if (!store.notice) return;
  showToast(store.notice);
  store.clearNotice();
}

watch(() => store.notice, drainNotice);
onMounted(drainNotice);

function run(id: number) {
  const cmd = store.items.find((i) => i.id === id);
  if (!cmd) return;
  if (cmd.need_confirm) {
    pendingRun.value = cmd;
    return;
  }
  return doRun(cmd);
}

async function doRun(cmd: CommandRow) {
  pendingRun.value = null;
  try {
    // goTerm 只在注入成功后才被调用：先跳页再失败，用户会站在终端页里
    // 等一行永远不会出现的命令。
    //
    // 这里不传 confirm：确认的判据是 cmd.need_confirm（后端落库的结论），
    // 由 store 一处决定。面板再传一遍就是第二个主人，而它传递的是"用户
    // 已经点过确认了"这种随时会被写错的东西。
    await store.run(cmd, { goTerm: () => router.push({ name: 'term' }) });
  } catch {
    // 失败原因由 store 写进 notice，下面的 watch 负责摆出来。
  }
  await store.refreshBusy();
}

async function move(id: number, dir: 'up' | 'down') {
  await store.move(id, dir).catch(() => undefined);
}

async function confirmDelete() {
  const cmd = pendingDelete.value;
  pendingDelete.value = null;
  if (!cmd) return;
  await store.remove(cmd.id);
}

const editingRow = () =>
  typeof formFor.value === 'number' ? store.items.find((i) => i.id === formFor.value) : undefined;
</script>

<template>
  <div class="cmds">
    <div v-if="store.error" class="errline">{{ store.error }}</div>

    <div class="cmdhint">
      <AppIcon name="info" :size="13" />
      <span>{{ store.runHint }}</span>
    </div>

    <CommandTile
      v-for="item in store.items"
      :key="item.id"
      :row="item"
      :pending="!!store.pending[item.id]"
      @run="run"
      @edit="formFor = $event"
      @remove="pendingDelete = store.items.find((i) => i.id === $event) ?? null"
      @move="move"
    />

    <div v-if="store.loaded && store.items.length === 0" class="empty">
      还没有快捷命令。添加一条常用命令，点一下就跳到终端里执行。
    </div>

    <div class="tile-add" role="button" @click="formFor = 'new'">
      <AppIcon name="add" :size="16" /> 添加命令
    </div>

    <AddCommandDialog
      v-if="formFor !== null"
      class="addcmd"
      :editing="editingRow()"
      @saved="formFor = null; store.load()"
      @close="formFor = null"
      @deleted="formFor = null; store.load()"
    />

    <!-- 危险命令的执行确认：只看后端算好的 need_confirm，前端不另判一套 -->
    <Sheet v-if="pendingRun" class="run-confirm" title="需要确认" icon="alert" @close="pendingRun = null">
      <div class="warnline">这条命令被标记为需要确认，确认后会直接注入终端会话执行。</div>
      <pre class="cmdline">{{ pendingRun.command }}</pre>
      <template #footer>
        <button class="btn btng btn-cancel" type="button" @click="pendingRun = null">取消</button>
        <button class="btn btnd btn-confirm" type="button" @click="doRun(pendingRun)">确认执行</button>
      </template>
    </Sheet>

    <Sheet v-if="pendingDelete" class="del-confirm" title="删除命令" icon="trash" @close="pendingDelete = null">
      <p>只移除面板里的这条记录，不会执行或删除任何文件。</p>
      <template #footer>
        <button class="btn btng btn-cancel" type="button" @click="pendingDelete = null">取消</button>
        <button class="btn btnd btn-confirm" type="button" @click="confirmDelete">确认删除</button>
      </template>
    </Sheet>

    <div v-if="toastMsg" class="toast">{{ toastMsg }}</div>
  </div>
</template>

<style scoped>
.cmdhint {
  display: flex;
  align-items: center;
  gap: 6px;
  font-size: 11px;
  color: var(--text-mute);
  padding: 0 2px 8px;
}
.empty {
  font-size: 12px;
  color: var(--text-mute);
  padding: 18px 0;
  text-align: center;
}
.tile-add {
  display: flex;
  align-items: center;
  justify-content: center;
  gap: 6px;
  padding: 10px;
  border: 1px dashed var(--border-strong, rgba(255, 255, 255, 0.12));
  border-radius: var(--radius-sm);
  font-size: 12px;
  color: var(--text-dim);
  cursor: pointer;
}
.tile-add:hover {
  color: var(--accent);
}
.cmdline {
  margin: 10px 0 0;
  padding: 10px 12px;
  background: rgba(0, 0, 0, 0.3);
  border-radius: 6px;
  font-family: var(--mono, ui-monospace, monospace);
  font-size: 12px;
  white-space: pre-wrap;
  word-break: break-all;
}
.toast {
  position: fixed;
  top: 22px;
  left: 50%;
  transform: translateX(-50%);
  padding: 10px 20px;
  background: rgba(20, 26, 33, 0.97);
  border: 1px solid var(--border-color);
  border-radius: var(--radius-sm);
  font-size: 12.5px;
  z-index: 9999;
  box-shadow: 0 8px 26px rgba(0, 0, 0, 0.5);
  pointer-events: none;
}
</style>
