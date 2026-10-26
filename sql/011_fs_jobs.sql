-- 目录移动 / 复制的后台作业。
--
-- 目录没有“一次性改名”的原语：key 就是路径，所以搬 N 个对象 = N 次服务端复制。
-- 这张表只给服务端看：对外没有“任务”这个概念，进度地址就是动作自己的子路径
-- （/move/{id}、/copy/{id}），所以表里一个 op 列区分两个动作就够。

do $$
begin
  if to_regclass('public.move_jobs') is not null
     and to_regclass('public.fs_jobs') is null then
    alter table move_jobs rename to fs_jobs;
  end if;
end $$;

create table if not exists fs_jobs (
  id                  text primary key,                -- m_<24 位十六进制> / c_<24 位>
  user_id             uuid not null references users(id) on delete cascade,
  op                  text not null default 'move',    -- move / copy
  from_path           text not null,
  to_path             text not null,
  status              text not null default 'pending', -- pending / running / done / failed / cancelled
  total               int  not null default 0,
  done                int  not null default 0,
  error               text,
  cancel_requested_at timestamptz,
  created_at          timestamptz not null default now(),
  started_at          timestamptz,
  finished_at         timestamptz
);

-- 老库从 move_jobs 改名过来的，补齐新列。
alter table fs_jobs add column if not exists op text not null default 'move';
alter table fs_jobs add column if not exists cancel_requested_at timestamptz;

-- 索引跟着表改名，避免每次迁移都多建一套。
alter index if exists move_jobs_user_time rename to fs_jobs_user_time;
alter index if exists move_jobs_active rename to fs_jobs_active;

create index if not exists fs_jobs_user_time on fs_jobs (user_id, created_at desc);
create index if not exists fs_jobs_active on fs_jobs (status) where status in ('pending', 'running');

-- 008 每次 migrate 都会把 move_jobs 重新 create 出来（if not exists）。
-- 如果它已经是空壳了，就清掉，别在库里留一张没用的表。
do $$
begin
  if to_regclass('public.fs_jobs') is not null
     and to_regclass('public.move_jobs') is not null
     and not exists (select 1 from move_jobs limit 1) then
    drop table move_jobs;
  end if;
end $$;
