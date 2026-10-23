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

-- 同一路径同时只能有一条存活记录（回收站里的可以重名）
create unique index if not exists nodes_path_live_uniq
  on nodes (path) where deleted_at is null;

create index if not exists nodes_parent_live
  on nodes (parent) where deleted_at is null;

-- 文件名模糊搜索
create index if not exists nodes_name_trgm
  on nodes using gin (name gin_trgm_ops);

-- 按标签 / 属性过滤
create index if not exists nodes_meta_gin
  on nodes using gin (meta jsonb_path_ops);

create index if not exists nodes_deleted
  on nodes (deleted_at) where deleted_at is not null;

create index if not exists nodes_size on nodes (size);

-- 回收站清理时按 key 找对象
create index if not exists nodes_key on nodes (key);
