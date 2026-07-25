-- 变更原因：费用比对按 upstream.protocol 决定是否从 input_tokens 里扣除 cached_tokens
--   （Claude 的 input_tokens 不含 cache_read，OpenAI 的 prompt_tokens 含）。
--   原实现在比对时 LEFT JOIN upstreams 现查当前协议，一旦后台改过渠道协议，
--   就会用新口径解释历史用量，input token 直接算错。
-- 影响范围：request_attempts 增加 protocol 列，写入时快照当次尝试的实际协议。
--   历史行留空，比对时回退到现查协议（与旧行为一致，不会更差）。
ALTER TABLE request_attempts ADD COLUMN IF NOT EXISTS protocol TEXT NOT NULL DEFAULT '';
