<script setup lang="ts">
import { ref } from 'vue';
import ServicesPanel from '../components/services/ServicesPanel.vue';
import CommandsPanel from '../components/quickcmd/CommandsPanel.vue';

// 快捷命令标签页：服务管理 + 快捷命令两个分段（原型 p-quick 的 seg）。
// 默认停在服务管理 —— 这一页的主用途是看服务在不在线。
const seg = ref<'svc' | 'cmd'>('svc');
</script>

<template>
  <div class="view quick">
    <div class="seg">
      <div class="segb" :class="{ on: seg === 'svc' }" role="button" @click="seg = 'svc'">
        服务管理
      </div>
      <div class="segb" :class="{ on: seg === 'cmd' }" role="button" @click="seg = 'cmd'">
        快捷命令
      </div>
    </div>

    <!-- v-if 而不是 v-show：切走就退订 services 频道，
         不在眼前的分段不该继续吃 WS 帧和渲染。 -->
    <ServicesPanel v-if="seg === 'svc'" class="svc-pane" />

    <!-- v-if 同上一个分段：切走就卸载，命令列表不该在看不见时继续拉。 -->
    <CommandsPanel v-if="seg === 'cmd'" />
  </div>
</template>

<style scoped>
.quick {
  padding: 10px;
}
.seg {
  display: flex;
  background: var(--card);
  border-radius: var(--radius-sm);
  padding: 3px;
  margin-bottom: 10px;
}
.segb {
  flex: 1;
  text-align: center;
  padding: 8px;
  font-size: 13px;
  color: var(--text-dim);
  border-radius: 6px;
  cursor: pointer;
}
.segb.on {
  background: var(--accent);
  color: #fff;
  font-weight: 600;
}
</style>
