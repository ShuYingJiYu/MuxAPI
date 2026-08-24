package store

// GORM model definitions — authoritative schema source for AutoMigrate.
// Field types and defaults are encoded via struct tags so both PostgreSQL
// and SQLite receive identical logical schemas.

type GroupModel struct {
	ID            int64    `gorm:"primaryKey;autoIncrement"`
	Name          string   `gorm:"type:text;not null"`
	Description   string   `gorm:"type:text;not null;default:''"`
	SortOrder     int      `gorm:"type:integer;not null;default:0"`
	MaxMultiplier *float64 `gorm:"type:real"`
}

func (GroupModel) TableName() string { return "groups" }

type UpstreamModel struct {
	ID           int64   `gorm:"primaryKey;autoIncrement"`
	Name         string  `gorm:"type:text;not null"`
	Source       string  `gorm:"type:text;not null;default:''"`
	BaseURL      string  `gorm:"column:base_url;type:text;not null"`
	APIKey       string  `gorm:"column:api_key;type:text;not null"`
	Proxy        string  `gorm:"type:text;not null;default:''"`
	Protocol     string  `gorm:"type:text;not null;default:'passthrough'"`
	BillingType  string  `gorm:"column:billing_type;type:text;not null;default:'none'"`
	CacheMode    string  `gorm:"column:cache_mode;type:text;not null;default:'auto'"`
	Enabled      bool    `gorm:"type:integer;not null;default:1"`
	ChannelProbe bool    `gorm:"column:channel_probe;type:integer;not null;default:1"`
	SortOrder    int     `gorm:"column:sort_order;type:integer;not null;default:0"`
	CreditRatio  float64 `gorm:"column:credit_ratio;type:real;not null;default:1"`
}

func (UpstreamModel) TableName() string { return "upstreams" }

type TagModel struct {
	ID        int64  `gorm:"primaryKey;autoIncrement"`
	Name      string `gorm:"type:text;not null"`
	Color     string `gorm:"type:text;not null;default:'gray'"`
	SortOrder int    `gorm:"column:sort_order;type:integer;not null;default:0"`
}

func (TagModel) TableName() string { return "tags" }

type UpstreamTagModel struct {
	UpstreamID int64 `gorm:"column:upstream_id;primaryKey"`
	TagID      int64 `gorm:"column:tag_id;primaryKey"`
	IsPrimary  bool  `gorm:"column:is_primary;type:integer;not null;default:0"`
}

func (UpstreamTagModel) TableName() string { return "upstream_tags" }

type GroupUpstreamModel struct {
	GroupID    int64 `gorm:"column:group_id;primaryKey"`
	UpstreamID int64 `gorm:"column:upstream_id;primaryKey"`
	Priority   int   `gorm:"type:integer;not null;default:50"`
	Weight     int   `gorm:"type:integer;not null;default:1"`
	Enabled    bool  `gorm:"type:integer;not null;default:1"`
}

func (GroupUpstreamModel) TableName() string { return "group_upstreams" }

type AccessKeyModel struct {
	ID      int64  `gorm:"primaryKey;autoIncrement"`
	Name    string `gorm:"type:text;not null;default:''"`
	Key     string `gorm:"type:text;not null;uniqueIndex"`
	GroupID int64  `gorm:"column:group_id;type:integer;not null"`
	Enabled bool   `gorm:"type:integer;not null;default:1"`
}

func (AccessKeyModel) TableName() string { return "access_keys" }

type MonitorModel struct {
	ID          int64  `gorm:"primaryKey;autoIncrement"`
	UpstreamID  int64  `gorm:"column:upstream_id;type:integer;not null"`
	Model       string `gorm:"type:text;not null"`
	Name        string `gorm:"type:text;not null;default:''"`
	Enabled     bool   `gorm:"type:integer;not null;default:1"`
	Stream      bool   `gorm:"type:integer;not null;default:0"`
	ProbeText   string `gorm:"column:probe_text;type:text;not null;default:''"`
	MaxTokens   int    `gorm:"column:max_tokens;type:integer;not null;default:0"`
	IntervalSec int    `gorm:"column:interval_sec;type:integer;not null;default:0"`
	Path        string `gorm:"type:text;not null;default:''"`
	Sort        int    `gorm:"type:integer;not null;default:0"`
}

func (MonitorModel) TableName() string { return "monitors" }

type LogModel struct {
	ID         int64  `gorm:"primaryKey;autoIncrement"`
	GroupID    int64  `gorm:"column:group_id;type:integer;not null"`
	UpstreamID int64  `gorm:"column:upstream_id;type:integer;not null"`
	Status     int    `gorm:"type:integer;not null"`
	LatencyMs  int64  `gorm:"column:latency_ms;type:integer;not null"`
	CreatedAt  int64  `gorm:"column:created_at;type:integer;not null"`
	Model      string `gorm:"type:text;not null;default:''"`
	Endpoint   string `gorm:"type:text;not null;default:''"`
	KeyName    string `gorm:"column:key_name;type:text;not null;default:''"`
	Retries    int    `gorm:"type:integer;not null;default:0"`
	ErrorText  string `gorm:"column:error_text;type:text;not null;default:''"`
}

func (LogModel) TableName() string { return "logs" }

type SettingModel struct {
	Key   string `gorm:"primaryKey;type:text"`
	Value string `gorm:"type:text;not null"`
}

func (SettingModel) TableName() string { return "settings" }
