-- 0006_fs_jobs: 后台文件任务（设计 8.4 / M6-T4）。
--
-- 存在的前提是"执行方是守护进程"：用户提交 copy/move/delete 后立即落库
-- 并返回 id，浏览器关掉也照跑（设计第 18 行与验收项都要求这一条，同步
-- HTTP 端点做不到 —— r.Context() 随关标签页取消，任务就地停住）。
--
-- op 只收 copy|move|delete，比设计 598 行的枚举窄，是有意的收窄：
--   - upload 已经有自己那套可续传协议（X-Upload-Id + 分块 + TTL 清扫），
--     再套一层 job 只会造出两个主人（谁决定"这个上传还活着"？）；
--   - rename / mkdir / zip 是即时操作，做成 job 只会给 UI 多加一次
--     "任务已提交" + 轮询，没有任何收益。
-- 需要后台化的判据不是"它是个端点"，而是"工作量随一个用户可控且无上限
-- 的量增长"：copy/move 随字节数、delete 随条目数与目录树深度（实测
-- 5000 个文件约 1.4s、清空 200 棵 200 文件的目录树约 1.2s）。
--
-- cancel_requested 是设计表里没有的一列，必须有：用户对**排队中**的任务
-- 点取消后面板若正好崩掉，取消意图不落盘的话，重启时队列会把这个用户
-- 已经明确说"别做"的 copy/move/**delete** 真的执行掉。

CREATE TABLE fs_jobs (
  id               INTEGER PRIMARY KEY,
  op               TEXT NOT NULL CHECK (op IN ('copy','move','delete')),
  -- src 是 JSON 数组（设计表写 TEXT，JSON 也是 TEXT）：delete/copy 都接受
  -- 多选自作一个任务，拆成多行会让"一次框选 500 个文件"变成 500 个任务，
  -- 而用户点的是一次删除。
  src              TEXT NOT NULL,
  dst              TEXT,                          -- delete 无目标
  total_bytes      INTEGER NOT NULL DEFAULT 0,
  done_bytes       INTEGER NOT NULL DEFAULT 0,
  entries_total    INTEGER NOT NULL DEFAULT 0,
  entries_done     INTEGER NOT NULL DEFAULT 0,
  state            TEXT NOT NULL DEFAULT 'pending'
    CHECK (state IN ('pending','running','done','failed','canceled','interrupted')),
  -- 用户是否要求取消。与 state 分开存：点下去时任务可能还在排队（state
  -- 仍是 pending），也可能正在跑（要靠 cancelFunc 打断），worker 观察到
  -- 之后才把 state 写成 canceled。
  cancel_requested INTEGER NOT NULL DEFAULT 0,
  error            TEXT,
  created_at       INTEGER NOT NULL,
  updated_at       INTEGER NOT NULL
);

-- 前端任务抽屉默认只看进行中/最近完成的，且每次打开都要按这个筛一遍。
CREATE INDEX idx_fs_jobs_state ON fs_jobs(state);
