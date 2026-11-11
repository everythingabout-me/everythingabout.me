-- A class A session is one of two shapes. `multipart` stages its bytes as parts under the
-- final key and the server publishes by completing them; `direct` lets the client PUT one
-- conditional object at the final key, so the file exists before the commit and the commit
-- only verifies and settles it. The mode is chosen when the session is created (from the
-- size and the connection's admitted conditional-write capability) and read back at commit
-- and abort; deriving it later would let a configuration edit change what an in-flight
-- session means.
--
-- Default keeps every row written by an older binary honest: those sessions all stage parts.
alter table file_uploads add column if not exists mode text not null default 'multipart';
