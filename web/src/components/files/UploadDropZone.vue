<script setup lang="ts">
import { onScopeDispose, ref } from 'vue';
import AppIcon from '../AppIcon.vue';
import { useUploadsStore } from '../../stores/uploads';

// 拖放上传区（设计 8.2：拖放区覆盖整个文件列表区域，拖入时高亮）。
//
// 它只负责"把 File 变成队列项"，不碰任何上传协议 —— 分块、续传、冲突
// 都在 stores/uploads 里。这一层的价值全在事件处理那几个细节上：
//
// · 必须给 window 也挂 dragover/drop 的 preventDefault。少挂一个的话,
//   用户手一抖掉在列表区域外面，浏览器会**直接打开这个文件**（图片跳到
//   新标签页、二进制跳到下载页），上传根本没开始，看起来像“面板坏了”。
//   挂在 window 而不是 document 上：拖到面包屑、工具栏、右边仪表上
//   都算“掉在外面”，逐个容器去挂必然会漏一个。
//
// · 进出计数（depth）而不是靠 dragleave 关高亮：拖过子元素时浏览器会
//   成对触发一堆 dragenter/dragleave，靠 dragleave 收工的话高亮会疯狂
//   闪烁。
const props = defineProps<{ dir: string }>();

const uploads = useUploadsStore();
const depth = ref(0);
const over = ref(false);

// File -> 队列项的数据源。切 Blob 而不是自己读进内存：Blob.slice 不
// 复制底层数据，一次读整个 800MB 文件进 ArrayBuffer 会直接把浏览器
// 标签页撑爆（这在一台 12GB 内存的机器上尤其明显）。
function toTask(f: File) {
  return {
    name: f.name,
    size: f.size,
    mtime: f.lastModified ? Math.round(f.lastModified / 1000) : 0,
    slice: (start: number, end: number) => f.slice(start, end),
  };
}

function onDrop(e: DragEvent) {
  e.preventDefault();
  depth.value = 0;
  over.value = false;
  const files = e.dataTransfer?.files;
  if (!files || files.length === 0) return;
  uploads.enqueue(Array.from(files).map(toTask));
}

function onEnter(e: DragEvent) {
  if (!hasFiles(e)) return; // 拖的是选中的文字/链接：不该假装能上传
  e.preventDefault();
  depth.value++;
  over.value = true;
}

// 非文件拖拽（选中文字、图片链接）进浏览器时也必须吞掉默认行为,
// 否则松开鼠标会把那段文字插进页面/把图片在新标签页打开。
function swallow(e: DragEvent) {
  e.preventDefault();
}

// 监听器在 setup 里直接挂、用 onScopeDispose 拆：本组件会随路由反复
// 挂载，不拆就会积下一堆 window handler（每个都在 preventDefault）。
//
// 为什么不放到 onMounted 里：那里调 onScopeDispose 依赖“生命周期钩子
// 执行时 currentInstance 仍指向本组件”这个实现细节，一旦 Vue 改了钩子的
// 调用时机，这里会静默退化成“从不拆监听器”。
if (typeof window !== 'undefined') {
  window.addEventListener('dragover', swallow);
  window.addEventListener('drop', swallow);
  onScopeDispose(() => {
    window.removeEventListener('dragover', swallow);
    window.removeEventListener('drop', swallow);
  });
}

function onLeave() {
  depth.value = Math.max(0, depth.value - 1);
  if (depth.value === 0) over.value = false;
}

function hasFiles(e: DragEvent): boolean {
  return Array.from(e.dataTransfer?.types ?? []).includes('Files');
}

// 目录拖进来时 files 里给的是条目对象而不是 File：当前只支持文件
// （目录上传要递归枚举 + 逐个建会话，是另一件事），所以不假装成功。
</script>

<template>
  <div
    class="dz"
    :class="{ over }"
    @dragenter="onEnter"
    @dragover.prevent
    @dragleave="onLeave"
    @drop="onDrop"
  >
    <slot />
    <div v-if="over" class="hint">
      <AppIcon name="upload" :size="26" />
      <div>松手上传到 {{ dir }}</div>
    </div>
  </div>
</template>

<style scoped>
.dz {
  position: relative;
  min-height: 100%;
}
.dz.over::after {
  content: '';
  position: absolute;
  inset: 0;
  border: 2px dashed var(--accent);
  border-radius: var(--radius);
  background: rgba(88, 166, 255, 0.08);
  pointer-events: none;
}
.hint {
  position: absolute;
  inset: 0;
  display: flex;
  flex-direction: column;
  align-items: center;
  justify-content: center;
  gap: 8px;
  font-size: 13px;
  color: var(--accent);
  pointer-events: none;
}
</style>
