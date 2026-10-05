// 与后端 internal/terminal/store.go 的 SessionMeta、internal/api/handlers_terminal.go
// 一一对应。后端是 snake_case（Go struct tag），这里保持同名：HTTP 与 WS
// 序列化的是同一个类型，前端自己改名就会与其中一条悄悄漂移。

export interface TermSessionRow {
  id: number;
  tmux_name: string;
  title: string;
  cwd: string;
  shell: string;
  history_limit: number;
  created_at: number;
  last_attached_at: number;
  // alive 由后端每次列表时问一次 tmux 得到。它不是"库里写着在"：
  // 用户在 tmux 里自己 kill-session 之后，只有 tmux 知道会话没了。
  alive: boolean;
  // 死因，仅 alive=false 有意义：>0 是进程退出码、-1 是"名字消失但读
  // 不到退出码"、0 是正常退出（后端会自动清理这种行）、null 是没记录
  // （活会话，或死因机制上线前留下的旧行）。
  exit_status: number | null;
}

export interface TermSessionInput {
  title: string;
  cwd?: string;
  shell?: string;
  history_limit?: number;
}

// 注意：后端目前**没有** term-events 频道，也没有任何形如
// {id, alive} / {id, reload} 的推送。这里绝不提前声明一个不存在的事件类型
// 让 store 去"处理"它 —— 那个处理器永远不会被调用，却会让人以为
// 会话状态是实时推送的。会话列表的新鲜度靠 load()/reload() 显式刷新。

// 与后端 historyLimitOptions 白名单一一对应。
export const HISTORY_LIMITS = [5000, 20000, 100000] as const;
// 默认档位故意不在前端硬编码：表单默认是"跟随面板默认"（history_limit
// 省略不发），由后端的 term_history_limit 设置决定。前端写死一个数字就
// 永远盖住那个设置，让它变成只存不读的摆设。

export const termChannel = (id: number) => `term:${id}`;
export const TERM_EVENTS_CHANNEL = 'term-events';
