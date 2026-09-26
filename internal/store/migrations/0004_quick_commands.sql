-- 0004_quick_commands: 快捷命令（设计 5.3 / D20 / M5-T8）。
--
-- 没有 timeout_ms（D20 移除）：命令注入终端执行，终止由用户 Ctrl-C 决定，
-- 面板不再代为超时杀进程。
-- 也没有 exec_history 表：注入的输出就是终端的输出，由 tmux scrollback
-- 保留（默认 20000 行），面板另存一份只会和终端里看到的不一致。

CREATE TABLE quick_commands (
  id           INTEGER PRIMARY KEY,
  name         TEXT NOT NULL,
  command      TEXT NOT NULL,
  cwd          TEXT,                              -- 注入前先 cd（可选）
  need_confirm INTEGER NOT NULL DEFAULT 0,        -- 危险命令由代码强制置 1
  sort         INTEGER NOT NULL DEFAULT 0,        -- 用户手动排序，见 List 的 ORDER BY
  created_at   INTEGER NOT NULL
);
