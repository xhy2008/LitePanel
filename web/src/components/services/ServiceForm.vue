<script setup lang="ts">
import { computed, reactive, ref } from 'vue';
import Sheet from './Sheet.vue';
import { getApi } from '../../api/inject';
import type { ServiceInput, ServiceRow, ServiceKind } from '../../api/services';

// 新增 / 编辑服务表单。
// editing 为 undefined 时走 POST，否则 PATCH —— 编辑只提交配置字段，
// 状态/PID/退出信息由后端维护，前端不碰。
const props = defineProps<{
  editing?: ServiceRow;
  initial?: ServiceInput;
  centered?: boolean;
}>();
const emit = defineEmits<{ saved: []; close: []; deleted: [] }>();

// 删除要确认：点一下就删掉服务和它的配置，代价比"少点一次确认"大得多。
// 两步之间按钮本身就变成确认按钮，不额外弹窗。
const armDelete = ref(false);

// 表单模型：全字段必填的空串起点。ServiceInput 里那些可选字段是为
// PATCH 设计的，直接拿来当 v-model 容器会让 .trim() 撞上 undefined。
interface FormModel {
  name: string;
  kind: ServiceKind;
  unit: string;
  start_cmd: string;
  stop_cmd: string;
  cwd: string;
  autostart: boolean;
  sort: number;
}

const blank: FormModel = {
  name: '', kind: 'command', unit: '', start_cmd: '', stop_cmd: '', cwd: '',
  autostart: false, sort: 0,
};

// 显式捣字段：直接展开 props.editing 会把 state/pid/exit_* 一起拖进表单模型，
// 那些是后端维护的运行时字段，混进来迟早会被当成可提交项。
const src = props.editing ?? props.initial;
const form = reactive<FormModel>({
  name: src?.name ?? blank.name,
  kind: src?.kind ?? blank.kind,
  unit: src?.unit ?? blank.unit,
  start_cmd: src?.start_cmd ?? blank.start_cmd,
  stop_cmd: src?.stop_cmd ?? blank.stop_cmd,
  cwd: src?.cwd ?? blank.cwd,
  autostart: src?.autostart ?? blank.autostart,
  sort: src?.sort ?? blank.sort,
});
const err = ref('');
const saving = ref(false);

// 校验在发请求之前：让后端拒绝虽然结果一样，但用户要等一个往返才知道
// "名字没填"，原因还被埋进一句 error 文案里。规则与后端 validateService 对齐。
const problem = computed(() => {
  if (!form.name.trim()) return '名称必填';
  if (form.kind === 'systemd' && !form.unit.trim()) return 'systemd 类型必须填单元名';
  if (form.kind === 'command' && !form.start_cmd.trim()) return '命令类型必须填启动命令';
  return '';
});

function askDelete() {
  armDelete.value = true;
}

function undoDelete() {
  armDelete.value = false;
}

function doDelete() {
  emit('deleted');
}

async function submit() {
  if (problem.value) {
    err.value = problem.value;
    return;
  }
  err.value = '';
  saving.value = true;
  const { api } = getApi();
  try {
    // 键名必须是 snake_case：后端 JSON 解码开了 DisallowUnknownFields，
    // 驼峰会被直接 400。
    const body: ServiceInput = {
      name: form.name.trim(),
      kind: form.kind,
      unit: form.kind === 'systemd' ? form.unit.trim() : '',
      start_cmd: form.kind === 'command' ? form.start_cmd : '',
      stop_cmd: form.kind === 'command' ? form.stop_cmd : '',
      cwd: form.cwd.trim(),
      autostart: form.autostart,
      sort: Number(form.sort) || 0,
    };
    if (props.editing) await api.patch(`/api/services/${props.editing.id}`, body);
    else await api.post('/api/services', body);
    emit('saved');
  } catch (e) {
    err.value = (e as Error).message || '保存失败';
  } finally {
    saving.value = false;
  }
}
</script>

<template>
  <Sheet :title="editing ? '编辑服务' : '添加服务'" icon="add" :centered="centered" @close="emit('close')">
    <form @submit.prevent="submit">
      <div class="fg">
        <label class="fl" for="svc-name">名称 <em class="req">*</em></label>
        <input id="svc-name" v-model="form.name" class="inp" name="name" placeholder="llama-server" />
      </div>

      <div class="fg">
        <span class="fl">类型</span>
        <select v-model="form.kind" class="inp" name="kind">
          <option value="command">command · 托管命令</option>
          <option value="systemd">systemd · 系统单元</option>
        </select>
      </div>

      <template v-if="form.kind === 'systemd'">
        <div class="fg">
          <label class="fl" for="svc-unit">单元 <em class="req">*</em></label>
          <input id="svc-unit" v-model="form.unit" class="inp" name="unit" placeholder="nginx" />
          <div class="hint">不带 .service 后缀也可以，面板按原样交给 systemctl。</div>
        </div>
      </template>

      <template v-else>
        <div class="fg">
          <label class="fl" for="svc-start">启动命令 <em class="req">*</em></label>
          <textarea
            id="svc-start"
            v-model="form.start_cmd"
            class="inp"
            name="start_cmd"
            rows="2"
            placeholder="/usr/local/bin/llama-server -m q4.gguf"
          />
          <div class="hint">经 sh -c 执行，可以用管道、重定向和 && 串联。</div>
        </div>
        <div class="fg">
          <label class="fl" for="svc-stop">自定义停止命令</label>
          <textarea id="svc-stop" v-model="form.stop_cmd" class="inp" name="stop_cmd" rows="2" />
          <div class="hint">
            留空则对整个进程组先 SIGTERM、宽限期后 SIGKILL。CLI 类服务常驻不响应 TERM 时填这个。
          </div>
        </div>
      </template>

      <div class="fg">
        <label class="fl" for="svc-cwd">工作目录</label>
        <input id="svc-cwd" v-model="form.cwd" class="inp" name="cwd" placeholder="/opt/app" />
        <div class="hint">相对路径按面板进程的目录解析；留空即用面板当前目录。</div>
      </div>

      <div class="fg">
        <label class="chk">
          <input v-model="form.autostart" type="checkbox" name="autostart" />
          开机自动启动
        </label>
      </div>

      <div v-if="armDelete" class="warnline">
        删除只移除面板里的记录与配置，不会动磁盘上的程序；正在运行的会先停掉。
      </div>
      <div v-if="err" class="errline">{{ err }}</div>
    </form>

    <!-- footer 必须是 Sheet 的直接子元素才会成为插槽：嵌在 form 里会被
         当成普通内容丢掉，底部两个按钮就没了。因此它落在 form 之外，
         保存按钮直接调 submit()（回车提交仍走 form 的 submit）。 -->
    <template #footer>
      <template v-if="editing && armDelete">
        <button class="btn btng btn-cancel-del" type="button" @click="undoDelete">取消</button>
        <button class="btn btnd btn-confirm-del" type="button" @click="doDelete">
          确认删除
        </button>
      </template>
      <template v-else>
        <button v-if="editing" class="btn btnd btn-del" type="button" @click="askDelete">
          删除
        </button>
        <button class="btn btng btn-cancel" type="button" @click="emit('close')">取消</button>
        <button class="btn btnp" type="button" :disabled="saving" @click="submit">
          {{ saving ? '保存中…' : '保存' }}
        </button>
      </template>
    </template>
  </Sheet>
</template>

<style scoped>
.fg {
  margin-bottom: 14px;
}
.fg:last-child {
  margin-bottom: 0;
}
.fl {
  display: block;
  font-size: 11.5px;
  color: var(--text-dim);
  margin-bottom: 6px;
  font-weight: 500;
}
.req {
  color: var(--err);
  font-style: normal;
}
.inp {
  width: 100%;
  background: var(--card-2);
  border: 1px solid var(--border);
  border-radius: var(--radius-sm);
  padding: 9px 10px;
  color: var(--text);
  font-size: 12.5px;
  font-family: inherit;
}
textarea.inp {
  resize: none;
  min-height: 56px;
  font-family: ui-monospace, Menlo, Consolas, monospace;
}
.hint {
  font-size: 10px;
  color: var(--text-mute);
  margin-top: 5px;
  line-height: 1.5;
}
.chk {
  display: flex;
  align-items: center;
  gap: 10px;
  font-size: 12.5px;
  color: var(--text-dim);
  cursor: pointer;
}
.errline {
  font-size: 11.5px;
  color: var(--err);
  margin-bottom: 10px;
}
.btn {
  border: none;
  border-radius: var(--radius-sm);
  padding: 9px 14px;
  font-size: 13px;
  cursor: pointer;
  font-family: inherit;
  display: inline-flex;
  align-items: center;
  justify-content: center;
  gap: 5px;
}
.btnp {
  background: var(--accent);
  color: #fff;
  font-weight: 600;
}
.btng {
  background: var(--card-2);
  color: var(--text-dim);
}
.btnd {
  background: rgba(255, 77, 79, 0.14);
  color: var(--err);
}
.warnline {
  font-size: 10.5px;
  color: var(--warn);
  background: rgba(245, 166, 35, 0.08);
  border: 1px solid rgba(245, 166, 35, 0.25);
  border-radius: var(--radius-sm);
  padding: 8px 10px;
  margin-bottom: 10px;
  line-height: 1.5;
}
.btn:disabled {
  opacity: 0.6;
  cursor: default;
}
</style>
