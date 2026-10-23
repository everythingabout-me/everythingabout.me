-- 密码改为可选：新账号默认没有密码，只用邮箱验证码登录。
-- 已有账号（如果有）保留原来的 password_hash，不受影响。
alter table users alter column password_hash drop not null;
