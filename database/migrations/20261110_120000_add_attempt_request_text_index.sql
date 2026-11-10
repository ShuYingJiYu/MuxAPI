-- Reason: legacy routing IDs may be TEXT while attempt IDs are UUID.
-- Match the comparison and ordering used to retrieve the latest attempt protocol.
CREATE INDEX IF NOT EXISTS idx_attempts_request_text
    ON request_attempts ((CAST(request_id AS TEXT)), attempt_no DESC);
