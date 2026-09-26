<script setup lang="ts">
import { computed, reactive, ref } from 'vue';
import Sheet from '../services/Sheet.vue';
import { useQuickCmdStore } from '../../stores/quickcmd';
import type { CommandRow } from '../../api/quickcmd';

// 新增 / 编辑快捷命令（设计 16.3：名称 + 命令内容，另有工作目录）。
//
// 危险命令的判定**不在这里**：normalized() 是唯一主人，它会把 rm -rf 这类
// 命令强制标成需要确认。表单只交用户勾的那个 need_confirm。
// 这里不许复制一套正则去"提示后端会强制确认"：两处规则一漂移，提示就成了
// 谎话，而这条谎话只在最要紧的命令上出现。要显示结论请看列表上的"确认"
// 角标，那个值来自后端。
const props = defineProps<{ editing?: CommandRow }>();
const emit = defineEmits<{ saved: []; close: []; deleted: [] }>();

const store = useQuickCmdStore();

const form = reactive({
  name: props.editing?.name ?? '',
  command: props.editing?.command ?? '',
  cwd: props.editing?.cwd ?? '',
  need_confirm: props.editing?.need_confirm ?? false,
});

const err = ref('');
const saving = ref(false);
// 删除两步确认：点一下就删掉配置，代价比"少点一次确认"大得多。
const armDelete = ref(false);

// 只为"空值"做即时反馈，不复制后端的危险判定：让后端拒绝虽然结果一样，
// 但用户要等一个往返才知道"名字没填"，原因还被埋进一句 error 文案里。
const problem = computed(() => {
  if (!form.name.trim()) return '名称必填';
  if (!form.command.trim()) return '命令内容必填';
  return '';
});

async function submit() {
  if (problem.value) {
    err.value = problem.value;
    return;
  }
  err.value = '';
  saving.value = true;
  const input = {
    name: form.name.trim(),
    command: form.command,
    cwd: form.cwd.trim(),
    need_confirm: form.need_confirm,
  };
  try {
    if (props.editing) await store.update(props.editing.id, input);
    else await store.create(input);
    emit('saved');
  } catch (e) {
    // 后端拒绝时必须把原因摆出来并留着抽屉：静默关掉等于告诉用户
    // "已经保存了"，而他回头发现列表里根本没有那条。
    err.value = (e as Error).message || '保存失败';
  } finally {
    saving.value = false;
  }
}
</script>

<template>
  <Sheet :title="editing ? '编辑快捷命令' : '添加快捷命令'" icon="add" @close="emit('close')">
    <form @submit.prevent="submit">
      <div class="fg">
        <label class="fl" for="cmd-name">名称 <em class="req">*</em></label>
        <input id="cmd-name" v-model="form.name" class="inp" name="name" placeholder="磁盘占用排行" />
      </div>

      <div class="fg">
        <label class="fl" for="cmd-cmd">命令 <em class="req">*</em></label>
        <textarea
          id="cmd-cmd"
          v-model="form.command"
          class="inp"
          name="command"
          rows="3"
          placeholder="du -sh /data/* | sort -h"
        />
        <div class="hint">点击后跳转到终端执行；当前会话都在忙时会自动新建会话。</div>
      </div>

      <div class="fg">
        <label class="fl" for="cmd-cwd">工作目录</label>
        <input id="cmd-cwd" v-model="form.cwd" class="inp" name="cwd" placeholder="/data/models" />
        <div class="hint">命令注入前会先 cd 到这里。相对路径按面板进程的目录解析。</div>
      </div>

      <div class="fg">
        <label class="chk">
          <input v-model="form.need_confirm" type="checkbox" name="need_confirm" />
          执行前二次确认
        </label>
      </div>

      <div v-if="armDelete" class="warnline">只移除面板里的这条记录，不会执行或删除任何文件。</div>
      <div v-if="err" class="errline">{{ err }}</div>
    </form>

    <template #footer>
      <template v-if="editing && armDelete">
        <button class="btn btng btn-cancel" type="button" @click="armDelete = false">取消</button>
        <button class="btn btnd btn-confirm-del" type="button" @click="emit('deleted')">确认删除</button>
      </template>
      <template v-else>
        <button v-if="editing" class="btn btnd btn-del" type="button" @click="armDelete = true">删除</button>
        <button class="btn btng btn-cancel" type="button" @click="emit('close')">取消</button>
        <button class="btn btnp btn-save" type="button" :disabled="saving" @click="submit">
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
</style>
