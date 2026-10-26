-- 分片上传：记住 COS 那边的 uploadId，complete 时用它合并分片。

alter table upload_sessions
  add column if not exists provider_upload_id text not null default '';
