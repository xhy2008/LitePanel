import { defineStore } from 'pinia';
import { getApi } from '../api/inject';

interface MeResponse {
  authenticated: boolean;
  must_change_password: boolean;
}

interface ApiErrorLike {
  code?: string;
  message?: string;
  retryAfter?: number;
}

interface LoginResponse {
  ok: boolean;
  must_change_password: boolean;
}

// 认证状态：loggedIn 与 must_change_password 的唯一来源。
// 后端的 must_change_password 闸门（403）也在这里归一化，界面据此弹强制改密框。
export const useAuthStore = defineStore('auth', {
  state: () => ({
    loggedIn: false,
    mustChangePassword: false,
    checking: false,
    busy: false,
    error: '',
    // 登录被 IP 锁定的截止时刻（epoch ms）；0 = 未锁定。
    lockedUntil: 0,
  }),
  getters: {
    locked: (s) => s.lockedUntil > 0 && Date.now() < s.lockedUntil,
  },
  actions: {
    async login(password: string): Promise<void> {
      const { api } = getApi();
      this.busy = true;
      this.error = '';
      try {
        const r = await api.post<LoginResponse>('/api/login', { password });
        this.loggedIn = true;
        this.mustChangePassword = !!r.must_change_password;
      } catch (e) {
        this.loggedIn = false;
        this.error = messageOf(e);
        const retryAfter = (e as ApiErrorLike)?.retryAfter;
        if (retryAfter) this.lockedUntil = Date.now() + retryAfter * 1000;
        else this.lockedUntil = 0;
        throw e;
      } finally {
        this.busy = false;
      }
    },

    async fetchMe(): Promise<void> {
      const { api } = getApi();
      this.checking = true;
      try {
        const r = await api.get<MeResponse>('/api/me');
        this.loggedIn = !!r.authenticated;
        this.mustChangePassword = !!r.must_change_password;
      } finally {
        this.checking = false;
      }
    },

    async changePassword(oldPw: string, newPw: string): Promise<void> {
      const { api } = getApi();
      this.busy = true;
      this.error = '';
      try {
        await api.post('/api/password', { old: oldPw, new: newPw });
        // 后端改密即吊销全部会话（含当前会话），本地必须同步清零，
        // 否则界面会停在“以为还登录着”的假象里。
        this.loggedIn = false;
        this.mustChangePassword = false;
      } catch (e) {
        this.error = messageOf(e);
        throw e;
      } finally {
        this.busy = false;
      }
    },

    async logout(): Promise<void> {
      const { api } = getApi();
      try {
        await api.post('/api/logout');
      } catch {
        // 服务端可能已经吊销了会话；本地一律清零，避免卡在半死状态。
      } finally {
        this.loggedIn = false;
        this.mustChangePassword = false;
      }
    },
  },
});

function messageOf(e: unknown): string {
  if (e && typeof e === 'object' && 'message' in e) {
    return String((e as { message: unknown }).message);
  }
  return String(e);
}
