-- Allow upstreams to declare a credit bonus ratio so the routing multiplier guard
-- stays accurate even when a channel tops up credits at a non-1:1 rate.
--
-- Example: a channel that gives 10 RMB of credits per 1 RMB paid has credit_ratio=10.
-- The effective routing multiplier = reported_multiplier / credit_ratio.
-- This prevents the max_multiplier gate from blocking cheap channels that report
-- a high raw group_ratio only because of their credit inflation scheme.
ALTER TABLE upstreams
    ADD COLUMN credit_ratio DOUBLE PRECISION NOT NULL DEFAULT 1;

ALTER TABLE upstreams
    ADD CONSTRAINT upstreams_credit_ratio_positive
    CHECK (credit_ratio > 0);
