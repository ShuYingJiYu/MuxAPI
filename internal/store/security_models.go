package store

const SecurityRetentionDays = 15

// SecurityLog 使用 Unix 秒，在 PostgreSQL 和隔离 SQLite 测试中保持相同结构。
// 同一分钟内相同来源、接口及结果合并；不包含凭据、正文或查询参数。
type SecurityLog struct {
	ID             int64  `json:"id" gorm:"primaryKey;autoIncrement"`
	AggregationKey string `json:"-" gorm:"type:text;not null;uniqueIndex"`
	FirstAt        int64  `json:"first_at" gorm:"not null"`
	LastAt         int64  `json:"last_at" gorm:"not null;index;index:idx_security_ip_time,priority:2;index:idx_security_success_time,priority:2"`
	Kind           string `json:"kind" gorm:"type:text;not null"`
	Success        bool   `json:"success" gorm:"not null;index:idx_security_success_time,priority:1"`
	ClientIP       string `json:"client_ip" gorm:"type:text;not null;index:idx_security_ip_time,priority:1"`
	PeerIP         string `json:"peer_ip" gorm:"type:text;not null"`
	Forwarded      bool   `json:"forwarded" gorm:"not null"`
	UserAgent      string `json:"user_agent" gorm:"type:text;not null"`
	Method         string `json:"method" gorm:"type:text;not null"`
	Endpoint       string `json:"endpoint" gorm:"type:text;not null"`
	Count          int64  `json:"count" gorm:"not null"`
	NewSource      bool   `json:"new_source" gorm:"not null"`
	NewBrowser     bool   `json:"new_browser" gorm:"not null"`
	AfterFailures  bool   `json:"after_failures" gorm:"not null"`
	Frequent       bool   `json:"frequent" gorm:"not null"`
}

func (SecurityLog) TableName() string { return "security_logs" }

type securityWrite struct {
	record  *SecurityLog
	barrier chan struct{}
}

type SecurityFilter struct {
	IP     string
	Result string
	Kind   string
	View   string
	From   int64
	To     int64
	Page   int
	Size   int
}

type SecuritySummary struct {
	Failures           int64 `json:"failures"`
	AttentionIPs       int64 `json:"attention_ips"`
	LatestSuccess      int64 `json:"latest_success"`
	LoginSuccesses     int64 `json:"login_successes"`
	ManagementAccesses int64 `json:"management_accesses"`
	LatestLogin        int64 `json:"latest_login"`
}

type SecurityPage struct {
	Entries []SecurityLog   `json:"entries"`
	Total   int64           `json:"total"`
	Summary SecuritySummary `json:"summary"`
	Page    int             `json:"page"`
	Size    int             `json:"size"`
	From    int64           `json:"from"`
	To      int64           `json:"to"`
}
