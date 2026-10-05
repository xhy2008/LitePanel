<script setup lang="ts">
import { computed, onMounted, ref } from 'vue';
import { useRouter } from 'vue-router';
import { useSettingsStore } from '../stores/settings';
import { GROUP_LABELS } from '../api/settings';
import type { SettingItem } from '../api/settings';
import Sheet from '../components/services/Sheet.vue';

// 设置页（M7-T6）。后端的每一项都带渲染所需的元数据（kind/min/max/enum/
// required/restart_required），视图只按元数据出控件，**不许**在这里再抄一份
// 键名或范围表 —— 那是"只存不读"以外的第二类漂移：后端收紧了范围，前端还
// 渲染着旧的，用户填得进去而后端拒收，错误只在按下保存之后才出现。
const store = useSettingsStore();
const router = useRouter();

onMounted(() => {
  if (!store.loaded) void store.load();
});

function labelOf(group: string) {
  // 未知分组回落显示原始名：宁可丑，也不要因为漏登记就把整块设置变不见。
  return GROUP_LABELS[group] ?? group;
}

const notice = computed(() => store.notice);

// 跳转登录后要清掉提示，否则回来还能看见上一条"已保存"。
function dismissed() {
  store.clearNotice();
}

async function save() {
  await store.save();
}

function discard() {
  store.dirty = {};
}

// ---- 撤销全部会话（危险操作，二次确认）----
const confirmOpen = ref(false);
async function doRevokeAll() {
  confirmOpen.value = false;
  const ok = await store.revokeAllSessions();
  if (ok) void router.push({ name: 'login' });
}
</script>

<template>
  <div class="view settings">
    <div v-if="store.loading && !store.loaded" class="st-hint">载入设置…</div>
    <div v-else-if="store.error && !store.loaded" class="st-err">{{ store.error }}</div>

    <template v-else>
      <nav class="st-nav" aria-label="设置分组">
        <a v-for="g in store.groups" :key="g.group" :href="`#g-${g.group}`">{{ labelOf(g.group) }}</a>
      </nav>

      <div v-if="notice" class="st-notice" @click="dismissed">{{ notice }}</div>
      <p v-if="store.error && store.loaded" class="st-err">{{ store.error }}</p>

      <section v-for="g in store.groups" :id="`g-${g.group}`" :key="g.group" class="st-group">
        <h2>{{ labelOf(g.group) }}</h2>
        <div v-for="it in g.items" :key="it.key" class="st-item">
          <div class="st-lab">
            <label :for="`f-${it.key}`">{{ it.label }}</label>
            <span v-if="it.restart_required" class="st-badge" title="重启面板后生效">需重启</span>
            <span v-if="it.overridden" class="st-badge ov" title="与默认值不同">已修改</span>
            <span v-if="it.key in store.dirty" class="st-badge dy">未保存</span>
          </div>

          <div class="st-ctl">
            <!-- 敏感项：值永不回显。留空 = 不修改（后端契约，见 handlers_settings.go）。 -->
            <template v-if="it.secret">
              <input
                :id="`f-${it.key}`"
                type="password"
                autocomplete="new-password"
                :placeholder="it.has_value ? '已设置（留空则不修改）' : '未设置'"
                :value="String(store.display(it.key) ?? '')"
                @input="store.edit(it.key, ($event.target as HTMLInputElement).value)"
              />
            </template>

            <!-- 枚举：只能取后端给的档位。 -->
            <template v-else-if="it.kind === 'enum'">
              <select
                :id="`f-${it.key}`"
                :value="store.display(it.key)"
                @change="store.edit(it.key, Number(($event.target as HTMLSelectElement).value))"
              >
                <option v-for="n in it.enum" :key="n" :value="n">{{ n }} {{ it.unit }}</option>
              </select>
            </template>

            <template v-else-if="it.kind === 'int'">
              <div class="st-num">
                <input
                  :id="`f-${it.key}`"
                  type="number"
                  :min="it.min"
                  :max="it.max"
                  :value="store.display(it.key)"
                  @input="store.edit(it.key, Number(($event.target as HTMLInputElement).value))"
                />
                <span class="st-unit">{{ it.unit }}</span>
              </div>
            </template>

            <template v-else>
              <input
                :id="`f-${it.key}`"
                type="text"
                :value="String(store.display(it.key) ?? '')"
                @input="store.edit(it.key, ($event.target as HTMLInputElement).value)"
              />
            </template>

            <button
              v-if="it.key in store.dirty"
              class="st-revert"
              @click="store.revert(it.key)"
            >撤销</button>
          </div>

          <!-- 后端把库里的坏值原样吐回并标 valid=false（不夹成 0）；这里如实告知，
               用户改掉它才能保存 —— 悄悄夹成一个"看起来合法"的值会把损坏固化。 -->
          <p v-if="!it.valid" class="st-bad">当前值不合法（{{ it.value }}），请改成有效值后保存</p>
        </div>
      </section>

      <!-- 危险操作区 -->
      <section class="st-group danger">
        <h2>危险操作</h2>
        <div class="st-item">
          <div class="st-lab"><label>注销所有登录会话</label></div>
          <div class="st-ctl">
            <button class="st-btn danger" @click="confirmOpen = true">注销全部</button>
          </div>
          <p class="st-hint">包括你当前这台设备，需要重新登录。</p>
        </div>
      </section>
    </template>

    <!-- 保存条：改动数写出来，用户要知道按下去会提交几项。 -->
    <div v-if="store.dirtyCount" class="st-bar">
      <span>{{ store.dirtyCount }} 项未保存</span>
      <div>
        <button class="st-btn" :disabled="store.saving" @click="discard">放弃改动</button>
        <button class="st-btn primary" :disabled="!store.canSave" @click="save">
          {{ store.saving ? '保存中…' : '保存' }}
        </button>
      </div>
    </div>

    <Sheet v-if="confirmOpen" title="注销所有登录会话？" @close="confirmOpen = false">
      <p>所有设备（包括你现在这台）都会退出登录，需要重新输入密码。</p>
      <template #footer>
        <button class="st-btn" @click="confirmOpen = false">取消</button>
        <button class="st-btn danger" @click="doRevokeAll">确认注销</button>
      </template>
    </Sheet>
  </div>
</template>





<style scoped>
.settings {
  padding: 12px;
  max-width: 720px;
  margin: 0 auto;
}
.st-nav {
  display: flex;
  gap: 8px;
  flex-wrap: wrap;
  margin-bottom: 14px;
  position: sticky;
  top: 0;
  background: var(--bg, #101014);
  padding: 8px 0;
  z-index: 2;
}
.st-nav a {
  font-size: 13px;
  padding: 4px 10px;
  border-radius: 999px;
  background: #1e1e26;
  color: #b9b9c7;
  text-decoration: none;
}
.st-group {
  background: #16161d;
  border: 1px solid #23232d;
  border-radius: 10px;
  padding: 12px 14px;
  margin-bottom: 14px;
  scroll-margin-top: 52px;
}
.st-group h2 {
  font-size: 14px;
  margin: 0 0 10px;
  color: #e6e6f0;
}
.st-group.danger {
  border-color: #5a2430;
}
.st-item {
  padding: 10px 0;
  border-top: 1px solid #22222c;
}
.st-item:first-of-type {
  border-top: none;
}
.st-lab {
  display: flex;
  align-items: center;
  gap: 6px;
  font-size: 13px;
  color: #d6d6e2;
  margin-bottom: 6px;
}
.st-ctl {
  display: flex;
  align-items: center;
  gap: 8px;
}
.st-ctl input,
.st-ctl select {
  flex: 1;
  min-width: 0;
  background: #101016;
  border: 1px solid #2a2a36;
  color: #e6e6f0;
  border-radius: 7px;
  padding: 7px 9px;
  font-size: 13px;
}
.st-num {
  display: flex;
  align-items: center;
  gap: 6px;
  flex: 1;
}
.st-unit {
  font-size: 12px;
  color: #8a8a9a;
}
.st-badge {
  font-size: 11px;
  padding: 1px 6px;
  border-radius: 4px;
  background: #2d2410;
  color: #d9a441;
}
.st-badge.ov {
  background: #12261b;
  color: #4fbf7f;
}
.st-badge.dy {
  background: #1b2340;
  color: #7aa2ff;
}
.st-bad {
  color: #ff7a7a;
  font-size: 12px;
  margin: 6px 0 0;
}
.st-hint {
  color: #8a8a9a;
  font-size: 12px;
  margin: 6px 0 0;
}
.st-err {
  color: #ff7a7a;
  font-size: 13px;
  margin: 8px 0;
}
.st-notice {
  background: #12261b;
  color: #7fe0a6;
  border: 1px solid #1f4b33;
  border-radius: 8px;
  padding: 8px 10px;
  font-size: 13px;
  margin-bottom: 12px;
}
.st-bar {
  position: sticky;
  bottom: 0;
  display: flex;
  justify-content: space-between;
  align-items: center;
  gap: 10px;
  background: #14141b;
  border: 1px solid #2a2a36;
  border-radius: 10px;
  padding: 10px 12px;
  margin-top: 14px;
  font-size: 13px;
  color: #c8c8d6;
}
.st-btn {
  background: #22222c;
  color: #d6d6e2;
  border: 1px solid #2f2f3b;
  border-radius: 7px;
  padding: 7px 12px;
  font-size: 13px;
  cursor: pointer;
}
.st-btn.primary {
  background: #2b5bd7;
  border-color: #2b5bd7;
  color: #fff;
}
.st-btn.danger {
  background: #7a2430;
  border-color: #7a2430;
  color: #fff;
}
.st-btn:disabled {
  opacity: 0.55;
  cursor: default;
}
.st-revert {
  background: none;
  border: none;
  color: #7aa2ff;
  font-size: 12px;
  cursor: pointer;
}
</style>
