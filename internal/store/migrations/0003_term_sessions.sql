-- 0003_term_sessions: 终端会话元数据（设计 568 行 / M5-T6）。
--
-- 只存"面板需要记住的东西"：标题、初始目录/shell、历史行数上限、是否还活着。
-- 终端的**内容**一个字节都不进数据库 —— scrollback 由 tmux 持有（D5），
-- 服务日志同理（D19）。这里存元数据是为了：面板重启后仍能把 tmux 里已有的
-- lp-* 会话还原成用户看得懂的名字。

CREATE TABLE term_sessions (
  id               INTEGER PRIMARY KEY,
  tmux_name        TEXT NOT NULL UNIQUE,        -- 'lp-<id>'
  title            TEXT,
  cwd              TEXT,
  shell            TEXT,
  history_limit    INTEGER NOT NULL DEFAULT 20000,
  created_at       INTEGER NOT NULL,
  last_attached_at INTEGER,
  -- tmux 会话被删（用户在 tmux 里 exit、或面板被 kill -9 后手工清理）时
  -- 对账置 0。留着行是为了 UI 能显示"这个会话已不存在"而不是凭空消失。
  alive            INTEGER NOT NULL DEFAULT 1
);
