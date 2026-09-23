<script setup lang="ts">
import AppIcon from '../AppIcon.vue';

// 底部弹出面板外壳（对齐原型 .sm/.sh/.shh/.shb/.shf）。
// 抽出来是为了日志抽屉和新增/编辑表单共用同一套进出场与遮罩行为。
withDefaults(defineProps<{ title: string; icon?: string }>(), { icon: 'info' });
const emit = defineEmits<{ close: [] }>();
</script>

<template>
  <!-- 单根容器：多根 fragment 会让父层 <ServiceForm class="svc-form"> 这类
       属性透传被静默丢弃，列表就没有可靠的钩子定位抽屉。两层都是
       position:fixed，用 display:contents 包一层不影响布局。 -->
  <div class="sheet">
  <div class="sm" @click="emit('close')" />
  <div class="sh" role="dialog" :aria-label="title">
    <div class="shh">
      <div class="sht"><AppIcon :name="icon" :size="17" />{{ title }}</div>
      <div class="shx" role="button" aria-label="关闭" @click="emit('close')">
        <AppIcon name="close" :size="18" />
      </div>
    </div>
    <div class="shb"><slot /></div>
    <div v-if="$slots.footer" class="shf"><slot name="footer" /></div>
  </div>
  </div>
</template>

<style scoped>
.sheet {
  /* 只是属性透传的落点，不参与布局（子元素全部 fixed 定位）。 */
  display: contents;
}
.sm {
  position: fixed;
  inset: 0;
  background: rgba(0, 0, 0, 0.6);
  z-index: 200;
  animation: fi 0.15s ease;
}
.sh {
  position: fixed;
  bottom: 0;
  left: 0;
  right: 0;
  background: var(--bg-elev);
  border-radius: 16px 16px 0 0;
  z-index: 201;
  transform: translateY(100%);
  animation: su 0.26s cubic-bezier(0.4, 0, 0.2, 1) forwards;
  /* 86% 而不是 100%：留出顶部一条背景，让人知道这是浮层、能下滑关掉。 */
  max-height: 86%;
  display: flex;
  flex-direction: column;
}
.shh {
  padding: 14px 16px;
  border-bottom: 1px solid var(--border-soft);
  display: flex;
  align-items: center;
  justify-content: space-between;
  flex-shrink: 0;
}
.sht {
  font-size: 14px;
  font-weight: 600;
  display: flex;
  align-items: center;
  gap: 7px;
}
.shx {
  color: var(--text-mute);
  cursor: pointer;
  display: flex;
}
.shb {
  padding: 14px 16px;
  overflow-y: auto;
}
.shf {
  padding: 12px 16px 16px;
  display: flex;
  gap: 8px;
  flex-shrink: 0;
}
.shf :deep(.btn) {
  flex: 1;
  padding: 13px;
}
@keyframes fi {
  from {
    opacity: 0;
  }
  to {
    opacity: 1;
  }
}
@keyframes su {
  to {
    transform: translateY(0);
  }
}
</style>
