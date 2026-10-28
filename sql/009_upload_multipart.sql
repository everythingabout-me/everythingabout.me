-- Multipart upload: remember the uploadId on the COS side and use it to merge parts on complete.

alter table upload_sessions
  add column if not exists provider_upload_id text not null default '';
