-- 0009_downloads: 下载任务的历史记录（设计 9.2 / M7-T2）。
--
-- 为什么面板要自己存一份：实测（dev/aria2hist）aria2 重启后 tellStopped
-- 从 2 条变成 **0 条** —— 终态记录跨重启一条都不留（--save-session 默认只
-- 存 active/waiting/paused）。所以"保留已完成/已失败的历史"这件事只有面板
-- 自己做得到，不是冗余缓存。
--
-- 这张表**只存两样东西**：提交时的事实（uri / name / dir / gid）与终态的
-- 事实（state / error / finished_at / 完成时的字节数）。
--
-- 刻意不存速度、也不周期性更新 completed_len：
--   - 进行中任务的实时状态由 aria2 独占（轮询器每 tick 拿），表里再存一份
--     就是两个主人 —— aria2 侧被别的客户端改动时两边必然漂移，而界面得挑
--     一个信，挑错了就显示假进度；
--   - 每个 tick 写一次 SQLite 也正好违背本里程碑对"面板开销极低"的承诺。
-- 完成那一刻的 total/completed 则是**已经不再变化**的历史事实，存下来才让
-- 历史列表能显示"这个文件多大"，而不必去猜（文件本身可能已经被用户移走）。
CREATE TABLE downloads (
  id              INTEGER PRIMARY KEY,
  gid             TEXT NOT NULL,
  -- uri 存 JSON 数组：aria2 的 addUri 接受同一文件的多个镜像地址，
  -- 拼成单串再拆会在 URL 含逗号时出错（查询串里逗号是合法字符）。
  uri             TEXT NOT NULL,
  name            TEXT,
  dir             TEXT,
  state           TEXT NOT NULL
    CHECK (state IN ('active','waiting','paused','complete','error','removed')),
  error           TEXT,
  total_len       INTEGER NOT NULL DEFAULT 0,
  completed_len   INTEGER NOT NULL DEFAULT 0,
  created_at      INTEGER NOT NULL,
  finished_at     INTEGER
);

-- 事件/轮询都要按 gid 找行，这是最高频的查询。
CREATE UNIQUE INDEX idx_downloads_gid ON downloads(gid);
-- 历史列表按时间倒序分页。
CREATE INDEX idx_downloads_created ON downloads(created_at DESC);
