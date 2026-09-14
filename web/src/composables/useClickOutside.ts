import { onMounted, onUnmounted, type Ref } from 'vue';

// 点击元素外部时触发（下拉菜单、右键菜单等共用）。
export function onClickOutside(el: Ref<HTMLElement | null>, handler: (e: Event) => void) {
  function onDoc(e: Event) {
    const node = el.value;
    if (!node) return;
    if (!node.contains(e.target as Node)) handler(e);
  }
  onMounted(() => document.addEventListener('click', onDoc, true));
  onUnmounted(() => document.removeEventListener('click', onDoc, true));
}
