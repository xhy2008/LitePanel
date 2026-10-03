<script setup lang="ts">
import { nextTick, ref, watch } from 'vue';
import AppIcon from '../AppIcon.vue';

// 地址栏（设计 8.1）。看起来只是"输入框 + 回车",但有一个只在使用中文
// 输入法时出现的坑：
//
// **候选词未上屏时的回车**同样会被浏览器报成 keydown Enter。如果这时
// 提交，用户敲回车本意是确认候选词，面板却把地址提交了，而且提交的还是
// 那串没打完的拼音。区分点是 e.isComposing。
//
// 也正因为如此，这里**不能**用 @keydown.enter.prevent 这种写法：修饰符
// 是无条件的 preventDefault，它会在"确认候选词"那一次也拦掉默认行为,
// 于是输入法根本选不上字 —— 比原来的问题更严重。preventDefault 必须
// 放在 isComposing 判断之后。
const props = defineProps<{ modelValue: string }>();
const emit = defineEmits<{ submit: [path: string]; blur: [] }>();

const text = ref(props.modelValue);
const editing = ref(false);
const input = ref<HTMLInputElement | null>(null);

// 外部换了目录（点面包屑、进目录）而用户没在编辑时，输入框要跟上。
// 正在编辑时**不能**覆盖：那时用户敲进去的内容比地址栏的旧值更有权。
watch(
  () => props.modelValue,
  (v) => {
    if (!editing.value) text.value = v;
  },
);

function start() {
  editing.value = true;
  void nextTick(() => input.value?.select());
}

function done() {
  editing.value = false;
  text.value = props.modelValue;
  emit('blur');
}

function onKeydown(e: KeyboardEvent) {
  if (e.key !== 'Enter') return;
  if (e.isComposing) return; // 输入法选词的回车：完全不碰它
  e.preventDefault();
  submit();
}

function submit() {
  const v = text.value.trim();
  editing.value = false;
  // 空输入或没改动就不发请求，也不让地址栏停在空白上。
  if (v && v !== props.modelValue) emit('submit', v);
  else text.value = props.modelValue;
}
</script>

<template>
  <div class="pi" :class="{ on: editing }">
    <AppIcon name="folder" :size="15" />
    <input
      v-if="editing"
      ref="input"
      v-model="text"
      class="in"
      spellcheck="false"
      autocapitalize="off"
      autocomplete="off"
      @keydown="onKeydown"
      @blur="done"
    />
    <div v-else class="txt" role="button" @click="start">{{ modelValue || '/' }}</div>
  </div>
</template>

<style scoped>
.pi {
  display: flex;
  align-items: center;
  gap: 6px;
  flex: 1 1 8rem;
  /* 地板宽度不能是 0。地址栏是唯一的「手动输入路径」入口：工具栏按钮一多
     （新建/粘贴/上传/回收站，再加排序那一组）就会把它挤到看不见，而初始
     目录不可读时连面包屑都没有 —— 结果是根本没地方开始。宁可让按钮那侧
     换行，也不能让导航入口消失。 */
  min-width: 8rem;
  padding: 6px 8px;
  border: 1px solid transparent;
  border-radius: 6px;
  color: var(--text-dim);
}
.pi.on {
  border-color: var(--accent);
  background: var(--bg);
}
.in {
  flex: 1;
  min-width: 0;
  background: transparent;
  border: 0;
  outline: 0;
  color: var(--text);
  font-size: 12px;
  font-family: ui-monospace, monospace;
}
.txt {
  flex: 1;
  min-width: 0;
  font-size: 12px;
  font-family: ui-monospace, monospace;
  white-space: nowrap;
  overflow: hidden;
  text-overflow: ellipsis;
  color: var(--text);
  cursor: text;
}
</style>
