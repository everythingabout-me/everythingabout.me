-- 彻底去掉「软删除 / 回收站」这套东西：覆盖和删除都是最终态，没有隐藏状态。
--
-- 升级前如果老库里还有 deleted_at 非空的记录，请先用旧版本跑一次
--   eam purge --days 0
-- 把对象也删掉。这里只删索引行；对象若留在桶里，reconcile 会把它扫回来。

delete from nodes where deleted_at is not null;

-- 依赖 deleted_at 的索引先拆掉
drop index if exists nodes_parent_live;
drop index if exists nodes_deleted;
drop index if exists nodes_owner_parent_live;
drop index if exists nodes_owner_path_live_uniq;
drop index if exists nodes_legacy_path_uniq;

alter table nodes drop column if exists deleted_at;

-- 重建不带软删除条件的索引
create unique index if not exists nodes_owner_path_uniq on nodes (owner_id, path);
create unique index if not exists nodes_legacy_path_uniq on nodes (path) where owner_id is null;
create index if not exists nodes_owner_parent on nodes (owner_id, parent);
create index if not exists nodes_parent on nodes (parent);
