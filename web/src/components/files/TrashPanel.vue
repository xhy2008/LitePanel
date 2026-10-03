<script setup lang="ts">
import { onMounted, ref } from 'vue';
import { useRouter } from 'vue-router';
import AppIcon from '../AppIcon.vue';
import Sheet from '../services/Sheet.vue';
import { useFilesStore } from '../../stores/files';
import { sizeLabel, mtimeLabel } from '../../api/files';
import type { TrashItem } from '../../api/files';

// 回收站面板（设计 8.6）。
//
// 列表项显示"原位置"而不是回收站里的真实路径：用户认得的是"我删的那个
// /data/backup"，而 .litepanel-trash/<id> 这串跟任何东西都对不上号。
//
// 恢复遇到同名文件时后端报错（它不猜要覆盖谁），这里原样转出后端文案 ——
// 编一句"恢复失败"会让人以为回收站坏了，而真正的原因是他得先把那个同名
// 文件挪走。
const emit = defineEmits<{ close: [] }>();

const store = useFilesStore();
const router = useRouter();

const items = ref<TrashItem[]>([]);
const loading = ref(false);
const busyId = ref('');
const err = ref('');

// "清空回收站"的二次确认用**两步按钮**而不是 window.confirm：
// 项目里已有这个约定（ServiceForm 的删除），而且原生 confirm 在 jsdom
// 里根本没法驱动 —— 用它的代价是这条最该测的路径测不了。
const armed = ref(false);

async function load() {
  loading.value = true;
  err.value = '';
  items.value = await store.loadTrash();
  loading.value = false;
}

onMounted(load);

async function restore(it: TrashItem) {
  busyId.value = it.id;
  err.value = '';
  try {
    await store.restoreTrash(it.id);
    items.value = await store.loadTrash();
  } catch (e) {
    err.value = (e as Error).message || '恢复失败';
  } finally {
    busyId.value = '';
  }
}

async function purge(it: TrashItem) {
  busyId.value = it.id;
  err.value = '';
  try {
    await store.purgeTrash(it.id);
    items.value = items.value.filter((x) => x.id !== it.id);
  } catch (e) {
    err.value = (e as Error).message || '删除失败';
  } finally {
    busyId.value = '';
  }
}

async function emptyAll() {
  err.value = '';
  try {
    await store.emptyTrash();
    armed.value = false;
    items.value = await store.loadTrash();
  } catch (e) {
    err.value = (e as Error).message || '清空失败';
  }
}

// 跳到原所在目录：恢复失败多半是同名冲突，让用户当场看到那个挡路的文件
// 比只读一句错误文案有用。
function reveal(it: TrashItem) {
  const i = it.origin.lastIndexOf('/');
  void store.open(i > 0 ? it.origin.slice(0, i) : '/');
  emit('close');
}
</script>

<template>
  <Sheet title="回收站" icon="trash" @close="emit('close')">
    <div v-if="err" class="err">{{ err }}</div>
    <div class="hint">文件删除后保留 3 天，到期自动清除</div>
    <div v-if="loading" class="muted">加载中…</div>
    <div v-else-if="!items.length" class="muted">回收站是空的</div>
    <div v-else class="list">
      <div v-for="it in items" :key="it.id" class="ti">
        <AppIcon :name="it.is_dir ? 'folder' : 'file'" :size="18" />
        <div class="meta">
          <div class="nm">{{ it.name }}</div>
          <div class="sub">原位置 {{ it.origin }} · {{ sizeLabel(it.size) }} · {{ mtimeLabel(it.deleted_at) }}</div>
        </div>
        <div class="ops">
          <button class="b" :disabled="busyId === it.id" @click="reveal(it)">查看位置</button>
          <button class="b" :disabled="busyId === it.id" @click="restore(it)">还原</button>
                    <button class="b d" :disabled="busyId === it.id" @click="purge(it)">删掉</button>
        </div>
      </div>
    </div>
    <template #footer>
      <div class="ft">
        <button class="b" @click="emit('close')">关闭</button>
        <button
          v-if="items.length && !armed"
          class="b d"
          @click="armed = true"
        >
          清空回收站
        </button>
        <template v-else-if="items.length">
          <button class="b" @click="armed = false">取消</button>
          <button class="b d" :disabled="loading" @click="emptyAll">确认清空</button>
        </template>
      </div>
    </template>
  </Sheet>
</template>

<style scoped>
.hint {
  font-size: 11px;
  color: var(--text-mute);
  margin-bottom: 10px;
}
.err {
  font-size: 12px;
  color: var(--err);
  margin-bottom: 8px;
}
.muted {
  font-size: 12px;
  color: var(--text-mute);
  padding: 16px 0;
  text-align: center;
}
.list {
  display: flex;
  flex-direction: column;
}
.ti {
  display: flex;
  align-items: center;
  gap: 8px;
  padding: 8px 0;
  border-bottom: 1px solid var(--border);
}
.meta {
  flex: 1;
  min-width: 0;
}
.nm {
  font-size: 13px;
  color: var(--text);
  white-space: nowrap;
  overflow: hidden;
  text-overflow: ellipsis;
}
.sub {
  font-size: 11px;
  color: var(--text-mute);
  white-space: nowrap;
  overflow: hidden;
  text-overflow: ellipsis;
}
.ops {
  display: flex;
  gap: 4px;
  flex-shrink: 0;
}
.b {
  background: transparent;
  border: 1px solid var(--border);
  border-radius: 5px;
  color: var(--text-dim);
  font-size: 11px;
  padding: 4px 8px;
  cursor: pointer;
}
.b:disabled {
  opacity: 0.5;
  cursor: default;
}
.b.d {
  color: var(--err);
}
.ft {
  display: flex;
  justify-content: space-between;
  gap: 8px;
  width: 100%;
}
</style>
