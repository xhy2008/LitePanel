<script setup lang="ts">
import { computed, ref } from 'vue';
import { useAuthStore } from '../stores/auth';

// 首次登录强制改密：初始密码只打印在 stderr 一次，改完即吊销全部会话。
const auth = useAuthStore();
const oldPw = ref('');
const newPw = ref('');
const confirm = ref('');
const done = ref(false);
const localError = ref('');

const mismatch = computed(() => confirm.value.length > 0 && newPw.value !== confirm.value);
const tooShort = computed(() => newPw.value.length > 0 && newPw.value.length < 8);
const canSubmit = computed(
  () => oldPw.value.length > 0 && newPw.value.length >= 8 && !mismatch.value && !auth.busy,
);

async function submit() {
  localError.value = '';
  if (newPw.value !== confirm.value) return; // 前端先拦住，不打无谓的请求
  if (newPw.value.length < 8) return;
  try {
    await auth.changePassword(oldPw.value, newPw.value);
    done.value = true;
  } catch (e) {
    localError.value = e && typeof e === 'object' && 'message' in e
      ? String((e as { message: unknown }).message)
      : String(e);
  }
}
</script>

<template>
  <div class="force-password">
    <form class="card" @submit.prevent="submit">
      <h2>修改初始密码</h2>
      <p class="hint">
        当前使用的是面板生成的一次性密码。请设置新密码；改密后所有会话（包括本机）都会被登出。
      </p>

      <label>
        当前密码
        <input v-model="oldPw" name="old" type="password" autocomplete="current-password" required />
      </label>
      <label>
        新密码（至少 8 位）
        <input v-model="newPw" name="new" type="password" autocomplete="new-password" required />
      </label>
      <label>
        确认新密码
        <input v-model="confirm" name="confirm" type="password" autocomplete="new-password" required />
      </label>

      <p v-if="tooShort" class="warn too-short">新密码至少 8 位</p>
      <p v-if="mismatch" class="warn mismatch">两次输入的新密码不一致</p>
      <p v-if="localError" class="warn server-error">{{ localError }}</p>
      <p v-if="done" class="ok reauth-hint">密码已修改，请使用新密码重新登录。</p>

      <button type="submit" :disabled="!canSubmit">修改密码</button>
    </form>
  </div>
</template>

<style scoped>
.force-password {
  display: grid;
  place-items: center;
  min-height: 100%;
  padding: 24px 16px;
}
.card {
  width: 100%;
  max-width: 380px;
  background: var(--card);
  border: 1px solid var(--border);
  border-radius: var(--radius-md);
  padding: 20px;
  display: flex;
  flex-direction: column;
  gap: 12px;
}
h2 {
  margin: 0;
  font-size: 17px;
}
.hint {
  margin: 0;
  font-size: 12px;
  color: var(--text-dim);
  line-height: 1.6;
}
label {
  display: flex;
  flex-direction: column;
  gap: 6px;
  font-size: 12px;
  color: var(--text-dim);
}
input {
  background: var(--bg);
  border: 1px solid var(--border);
  border-radius: var(--radius);
  color: var(--text);
  padding: 10px 12px;
  font-size: 14px;
}
input:focus {
  outline: none;
  border-color: var(--accent);
}
.warn {
  margin: 0;
  font-size: 12px;
  color: var(--warn);
}
.ok {
  margin: 0;
  font-size: 12px;
  color: var(--ok);
}
button {
  margin-top: 4px;
  padding: 10px 14px;
  border: 0;
  border-radius: var(--radius);
  background: var(--accent);
  color: #fff;
  font-size: 14px;
  cursor: pointer;
}
button:disabled {
  opacity: 0.5;
  cursor: not-allowed;
}
</style>
