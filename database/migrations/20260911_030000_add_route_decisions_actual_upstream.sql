-- Add actual_upstream_id to route_decisions so failover attempts can record
-- which upstream actually served the successful attempt without discarding
-- the initial pick's selected_upstream_id / candidate evaluations.
ALTER TABLE route_decisions
    ADD COLUMN IF NOT EXISTS actual_upstream_id BIGINT NOT NULL DEFAULT 0;
