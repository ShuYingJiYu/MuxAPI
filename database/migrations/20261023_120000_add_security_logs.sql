-- 原因：管理后台需要观察登录验证及管理 API 的异常访问。
-- 需求：来源、结果、重复合并与异常提示；保留 15 天，不保存凭据。
-- 影响范围：仅新增 security_logs 表及查询索引，不修改现有业务数据。
CREATE TABLE IF NOT EXISTS security_logs (
    id BIGSERIAL PRIMARY KEY,
    aggregation_key TEXT NOT NULL UNIQUE,
    first_at BIGINT NOT NULL,
    last_at BIGINT NOT NULL,
    kind TEXT NOT NULL,
    success BOOLEAN NOT NULL,
    client_ip TEXT NOT NULL,
    peer_ip TEXT NOT NULL,
    forwarded BOOLEAN NOT NULL,
    user_agent TEXT NOT NULL,
    method TEXT NOT NULL,
    endpoint TEXT NOT NULL,
    count BIGINT NOT NULL,
    new_source BOOLEAN NOT NULL,
    new_browser BOOLEAN NOT NULL,
    after_failures BOOLEAN NOT NULL,
    frequent BOOLEAN NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_security_logs_last_at ON security_logs(last_at);
CREATE INDEX IF NOT EXISTS idx_security_ip_time ON security_logs(client_ip, last_at);
CREATE INDEX IF NOT EXISTS idx_security_success_time ON security_logs(success, last_at);
