<script setup lang="ts">
import { computed, onUnmounted, ref } from 'vue';
import { useRoute, useRouter } from 'vue-router';
import { useAuthStore } from '../stores/auth';
import { useBreakpoint } from '../composables/useBreakpoint';
import { metricsState } from '../composables/useMetrics';
import MetricsPanel from '../components/metrics/MetricsPanel.vue';
import MetricTicks from '../components/metrics/MetricTicks.vue';
import { tickBars } from '../composables/metricsTicks';

// 登录页：单密码框；锁定倒计时来自后端 Retry-After。
const auth = useAuthStore();
const route = useRoute();
const router = useRouter();
const pw = ref('');

const tick = ref(0);
const locked = computed(() => auth.locked);
const lockedText = computed(() => {
  void tick.value; // 依赖倒计时心跳，文案才会逐秒走
  const left = Math.max(0, Math.ceil((auth.lockedUntil - Date.now()) / 1000));
  if (!left) return '';
  const m = Math.ceil(left / 60);
  return left > 60 ? `请 ${m} 分钟后再试` : `请 ${left} 秒后再试`;
});
const canSubmit = computed(() => pw.value.length > 0 && !auth.busy && !locked.value);

// 每秒刷新一次倒计时；未锁定就不留定时器。
let timer: ReturnType<typeof setInterval> | null = null;
function ensureTimer() {
  if (timer || !auth.lockedUntil) return;
  timer = setInterval(() => {
    tick.value += 1;
    if (Date.now() >= auth.lockedUntil && timer) {
      clearInterval(timer);
      timer = null;
      auth.lockedUntil = 0;
    }
  }, 1000);
}
onUnmounted(() => timer && clearInterval(timer));

// 只接受站内绝对路径，堵住 ?next=//evil.example.com 这种开放重定向。
function safeNext(): string {
  const next = route.query.next;
  if (typeof next === 'string' && next.startsWith('/') && !next.startsWith('//')) {
    return next;
  }
  return '/';
}

async function submit() {
  if (!canSubmit.value) return;
  ensureTimer();
  try {
    await auth.login(pw.value);
    router.replace(safeNext());
  } catch {
    // 错误文案已在 store.error；锁定时刻同步起倒计时。
    ensureTimer();
  }
}

// 登录页也显示实时指标（用户要求：监控 API 公开，输密码前先瞄一眼机器）。
// 数据不自己拉 —— App.vue 挂载时已统一 attachMetrics，这里只读共享单例。
// 宽度与外壳一致：手机给顶栏竖条，平板/PC 给右侧 128px 仪表栏。
const { bp } = useBreakpoint();
const showRail = computed(() => bp.value !== 'phone');

const loginTicks = computed(() => tickBars(metricsState.value.snapshot));
</script>

<template>
  <div class="login">
    <form class="card" @submit.prevent="submit">
      <div class="brand">LitePanel</div>
      <input
        v-model="pw"
        name="password"
        type="password"
        autocomplete="current-password"
        placeholder="密码"
        :disabled="auth.busy || locked"
      />
      <p v-if="auth.error" class="error">{{ auth.error }}{{ locked ? '（' + lockedText + '）' : '' }}</p>
      <button type="submit" :disabled="!canSubmit">
        {{ auth.busy ? '登录中…' : '登录' }}
      </button>
    </form>

    <!-- 登录前的实时指标：手机端用顶栏竖条，平/PC 用与主界面同规格的仪表栏 -->
    <div class="login-dash">
      <MetricTicks v-if="!showRail" :bars="loginTicks" />
      <MetricsPanel v-else :snapshot="metricsState.value.snapshot" />
    </div>
  </div>
</template>

<style scoped>
.login {
  min-height: 100%;
  display: grid;
  place-items: center;
  padding: 24px;
  gap: 24px;
}
/* 手机端：竖条放在卡片上方，跟主界面的顶部位置一致 */
.login:has(.login-dash) {
  grid-template-rows: auto 1fr;
  align-content: center;
  justify-items: center;
}
.login-dash {
  display: flex;
  align-items: center;
  justify-content: center;
}
/* 平/PC：仪表栏给足 128px 窄栏宽度，与原型 .rail 同宽 */
@media (min-width: 768px) {
  .login {
    grid-template-columns: 1fr;
  }
  .login-dash {
    width: var(--rail-w);
    max-width: 100%;
  }
}
.card {
  width: 100%;
  max-width: 320px;
  display: flex;
  flex-direction: column;
  gap: 12px;
  background: var(--card);
  border: 1px solid var(--border);
  border-radius: var(--radius-md);
  padding: 24px;
}
.brand {
  font-size: 18px;
  font-weight: 700;
  color: var(--accent);
  text-align: center;
  margin-bottom: 8px;
}
input {
  background: var(--bg);
  border: 1px solid var(--border);
  border-radius: var(--radius);
  color: var(--text);
  padding: 11px 12px;
  font-size: 14px;
}
input:focus {
  outline: none;
  border-color: var(--accent);
}
.error {
  margin: 0;
  font-size: 12px;
  color: var(--err);
}
button {
  padding: 11px;
  border: 0;
  border-radius: var(--radius);
  background: var(--accent);
  color: #fff;
  font-size: 14px;
  cursor: pointer;
}
button:disabled {
  opacity: 0.55;
  cursor: not-allowed;
}
</style>
