-- 个人数据基座：文件索引表
-- 对象字节留在 COS，这张表只负责"目录树 + 属性 + 检索"。

create extension if not exists pg_trgm;
create extension if not exists pgcrypto;

create table if not exists nodes (
  id            uuid primary key default gen_random_uuid(),
  key           text not null unique,              -- COS 对象 key，如 drive/Photos/a.jpg
  path          text not null,                     -- 逻辑路径，如 Photos/a.jpg（NFC 归一化）
  parent        text not null default '',          -- 父目录路径，根为 ''
  name          text not null,
  is_dir        boolean not null default false,
  size          bigint not null default 0,
  mime          text,
  etag          text,
  storage_class text,
  created_at    timestamptz not null default now(),
  updated_at    timestamptz not null default now(),
  deleted_at    timestamptz,                       -- 软删除，非空即在回收站
  meta          jsonb not null default '{}'::jsonb -- 标签 / EXIF / 自定义属性
);

-- 路径唯一性由 002_users.sql 按用户建立；这里不建全局唯一索引，
-- 否则多用户出现同名路径后重跑迁移会失败。
--
-- 010 已经把 deleted_at 删掉了。为了这套 SQL 可以重复执行（`make migrate` 每次都
-- 从头跑），依赖 deleted_at 的索引要按列是否存在来决定建不建。
do $$
begin
  if exists (
    select 1 from information_schema.columns
     where table_schema = 'public' and table_name = 'nodes' and column_name = 'deleted_at'
  ) then
    create index if not exists nodes_parent_live
      on nodes (parent) where deleted_at is null;
    create index if not exists nodes_deleted
      on nodes (deleted_at) where deleted_at is not null;
  end if;
end $$;

-- 文件名模糊搜索
create index if not exists nodes_name_trgm
  on nodes using gin (name gin_trgm_ops);

-- 按标签 / 属性过滤
create index if not exists nodes_meta_gin
  on nodes using gin (meta jsonb_path_ops);

create index if not exists nodes_size on nodes (size);

-- 回收站清理时按 key 找对象
create index if not exists nodes_key on nodes (key);
