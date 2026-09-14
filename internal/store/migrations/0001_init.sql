-- 0001_init: 基础表。其余表随各自里程碑的迁移文件加入。
-- 设计文档第 13 节：sessions 与 settings。

CREATE TABLE settings (
  key        TEXT PRIMARY KEY,
  value      TEXT NOT NULL,
  updated_at INTEGER NOT NULL
);

CREATE TABLE sessions (
  token_hash TEXT PRIMARY KEY,
  created_at INTEGER NOT NULL,
  expires_at INTEGER NOT NULL,
  user_agent TEXT,
  last_ip    TEXT,
  last_seen  INTEGER
);

CREATE INDEX idx_sessions_expires_at ON sessions(expires_at);
