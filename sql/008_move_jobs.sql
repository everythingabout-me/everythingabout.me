-- 目录移动的后台任务。
--
-- 目录没有“一次性改名”的原语：key 就是路径，所以移动 N 个文件 = N 次服务端复制。
-- 这个表记录进度，让 POST /move 立刻返回、客户端再轮询 GET /move/{id}。

create table if not exists move_jobs (
  id          text primary key,                 -- m_<24 位十六进制>
  user_id     uuid not null references users(id) on delete cascade,
  from_path   text not null,
  to_path     text not null,
  status      text not null default 'pending',  -- pending / running / done / failed
  total       int  not null default 0,
  done        int  not null default 0,
  error       text,
  created_at  timestamptz not null default now(),
  started_at  timestamptz,
  finished_at timestamptz
);

create index if not exists move_jobs_user_time on move_jobs (user_id, created_at desc);
create index if not exists move_jobs_active on move_jobs (status) where status in ('pending', 'running');
