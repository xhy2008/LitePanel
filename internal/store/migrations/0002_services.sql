-- 0002_services: 设计与显存服务托管（设计 6 节 / M4-T1）。
-- D19：服务日志不进数据库，只存内存环形缓冲。
-- D20：快捷命令改为注入终端执行，不存在 exec_history 表。

CREATE TABLE services (
  id          INTEGER PRIMARY KEY,
  name        TEXT NOT NULL UNIQUE,
  kind        TEXT NOT NULL,               -- 'systemd' | 'command'
  unit        TEXT,                        -- systemd 类型：单元名
  start_cmd   TEXT,                        -- command 类型：启动命令
  stop_cmd    TEXT,                        -- command 类型：可选自定义停止命令
  cwd         TEXT,                        -- command 类型：工作目录
  autostart   INTEGER NOT NULL DEFAULT 0,
  sort        INTEGER NOT NULL DEFAULT 0,
  created_at  INTEGER NOT NULL
);

-- command 类型服务的运行时状态。面板重启后 state 一律重置为 stopped（D11），
-- 但上次留下的退出信息保留，重启后仍能看到"异常退出 code 137"。
-- exit_reason 非空即代表"有一次退出记录"，因此退出码可以是合法的 0。
CREATE TABLE service_state (
  service_id   INTEGER PRIMARY KEY REFERENCES services(id) ON DELETE CASCADE,
  pid          INTEGER NOT NULL DEFAULT 0,
  pgid         INTEGER NOT NULL DEFAULT 0,
  started_at   INTEGER,
  state        TEXT NOT NULL DEFAULT 'stopped',  -- 'running'|'stopped'|'starting'|'stopping'
  exit_code    INTEGER,
  exit_signal  INTEGER NOT NULL DEFAULT 0,
  exit_reason  TEXT,                             -- 'clean' | 'error'（D21 只有这两种）
  exited_at    INTEGER,
  stopped_by   TEXT                              -- 'user'|'panel-shutdown'|'pdeathsig'|'self'
);
