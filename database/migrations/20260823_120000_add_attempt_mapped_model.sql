-- Record which model name was actually sent to upstream when a mapping applied.
ALTER TABLE request_attempts ADD COLUMN mapped_model TEXT NOT NULL DEFAULT '';
