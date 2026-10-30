-- Reason: retain per-attempt outbound estimates and upstream usage quality.
-- Scope: requests, request_attempts and route_decisions; existing rows remain unchanged.
ALTER TABLE requests ADD COLUMN IF NOT EXISTS token_accounting TEXT NOT NULL DEFAULT '';
ALTER TABLE request_attempts ADD COLUMN IF NOT EXISTS token_accounting TEXT NOT NULL DEFAULT '';
ALTER TABLE route_decisions ADD COLUMN IF NOT EXISTS token_accounting TEXT NOT NULL DEFAULT '';
