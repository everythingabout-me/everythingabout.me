-- Background jobs for directory moves.
--
-- There is no "rename in one shot" primitive for a directory: the key is the path, so moving N
-- files costs N server-side copies. This table tracks progress so POST /move can return
-- immediately and the client polls GET /move/{id}.

create table if not exists move_jobs (
  id          text primary key,                 -- m_<24 hex chars>
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
