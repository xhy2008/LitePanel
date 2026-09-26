// 与后端 internal/quickcmd/store.go 的 Command、internal/api/handlers_commands.go
// 的 run/busy 响应一一对应。后端是 snake_case（Go struct tag），这里保持同名：
// 前端自己改名就会与 HTTP 返回的字段悄悄漂移。

export interface CommandRow {
  id: number;
  name: string;
  command: string;
  cwd: string;
  // 危险命令的强制确认在后端 normalized() 里判定并落库，这里读到的就是
  // 最终结论。前端不许自己再判一次：两处规则一漂移，就会出现"前端不问、
  // 后端 400"这种点下去只有一句"失败"的故障。
  need_confirm: boolean;
  sort: number;
  created_at: number;
}

export interface CommandInput {
  name: string;
  command: string;
  cwd?: string;
  need_confirm?: boolean;
}

// POST /api/commands/{id}/run 的响应：前端跳终端页、选标签、决定提示文案
// 的全部依据。
export interface RunResult {
  session_id: number;
  is_new_session: boolean;
  title: string;
}

// GET /api/commands/busy 里的一项。后端只答它查到的会话：查不到的项不会
// 出现，所以"这里没有某个会话"意思是"不知道"，不是"空闲"。
export interface BusyEntry {
  session_id: number;
  busy: boolean;
  foreground: string;
  shell_name: string;
}
