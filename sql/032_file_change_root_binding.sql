-- Bind each owner's change stream to one explicit root configuration. Empty is
-- upgraded in place on first BindRoot; mismatches require an operator reset.
-- Reapplying migrations never rebinds roots or resets epochs/positions/history.
alter table file_change_heads add column if not exists root_namespace text not null default '';
