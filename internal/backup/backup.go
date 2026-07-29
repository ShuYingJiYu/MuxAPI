// Package backup provides scheduled and on-demand PostgreSQL backup to S3-compatible storage.
package backup

// S3Config holds credentials for an S3-compatible object store (e.g. Cloudflare R2).
type S3Config struct {
	Endpoint       string `json:"endpoint"`
	Region         string `json:"region"`
	Bucket         string `json:"bucket"`
	AccessKeyID    string `json:"access_key_id"`
	SecretKey      string `json:"secret_key,omitempty"` // omitted on GET when already set
	Prefix         string `json:"prefix"`
	ForcePathStyle bool   `json:"force_path_style"`
}

// Schedule controls the automatic backup cron job.
type Schedule struct {
	Enabled     bool   `json:"enabled"`
	CronExpr    string `json:"cron_expr"`    // standard 5-field cron, e.g. "0 3 * * *"
	RetainDays  int    `json:"retain_days"`  // delete backups older than N days (0 = keep forever)
	RetainCount int    `json:"retain_count"` // keep at most N backups (0 = unlimited)
}

// Record captures the outcome of one backup run.
type Record struct {
	ID          string `json:"id"`
	Status      string `json:"status"`                 // pending | running | completed | failed
	FileName    string `json:"file_name"`
	S3Key       string `json:"s3_key"`
	SizeBytes   int64  `json:"size_bytes,omitempty"`
	TriggeredBy string `json:"triggered_by"`           // "manual" | "scheduled"
	Error       string `json:"error,omitempty"`
	StartedAt   int64  `json:"started_at"`
	FinishedAt  int64  `json:"finished_at,omitempty"`
}
