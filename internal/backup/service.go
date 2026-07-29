package backup

import (
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/robfig/cron/v3"
)

const (
	keyS3Config  = "backup_s3_config"
	keySchedule  = "backup_schedule"
	keyRecords   = "backup_records"
	maxRecords   = 50
	presignExpiry = 15 * time.Minute
)

// Storage is the subset of the store needed by the backup service.
type Storage interface {
	GetSetting(key, def string) string
	SetSetting(key, value string) error
}

// Service orchestrates pg_dump → gzip → S3 upload and cron scheduling.
type Service struct {
	storage Storage
	dbURL   string

	mu      sync.Mutex
	running bool // prevent concurrent backup runs

	cron   *cron.Cron
	cronID cron.EntryID
}

// NewService creates a backup service. Call Run to start the scheduler.
func NewService(storage Storage, dbURL string) *Service {
	return &Service{
		storage: storage,
		dbURL:   dbURL,
		cron:    cron.New(),
	}
}

// Run starts the cron scheduler and blocks until ctx is cancelled.
func (s *Service) Run(ctx context.Context) {
	if err := s.syncCron(); err != nil {
		slog.Warn("backup: failed to restore cron schedule", "err", err)
	}
	s.cron.Start()
	<-ctx.Done()
	<-s.cron.Stop().Done()
}

func (s *Service) syncCron() error {
	sch, err := s.GetSchedule()
	if err != nil || !sch.Enabled || sch.CronExpr == "" {
		return err
	}
	return s.applyCron(sch.CronExpr)
}

func (s *Service) applyCron(expr string) error {
	if s.cronID != 0 {
		s.cron.Remove(s.cronID)
		s.cronID = 0
	}
	if expr == "" {
		return nil
	}
	id, err := s.cron.AddFunc(expr, func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
		defer cancel()
		if _, err := s.StartBackup(ctx, "scheduled"); err != nil {
			slog.Warn("backup: scheduled run failed", "err", err)
		}
	})
	if err != nil {
		return fmt.Errorf("invalid cron expression: %w", err)
	}
	s.cronID = id
	return nil
}

// --- Config ---

func (s *Service) GetS3Config() (S3Config, bool, error) {
	raw := s.storage.GetSetting(keyS3Config, "")
	if raw == "" {
		return S3Config{}, false, nil
	}
	var cfg S3Config
	if err := json.Unmarshal([]byte(raw), &cfg); err != nil {
		return S3Config{}, false, err
	}
	return cfg, true, nil
}

func (s *Service) SetS3Config(cfg S3Config) error {
	b, err := json.Marshal(cfg)
	if err != nil {
		return err
	}
	return s.storage.SetSetting(keyS3Config, string(b))
}

func (s *Service) TestConnection(ctx context.Context, cfg S3Config) error {
	store, err := newS3Store(ctx, cfg)
	if err != nil {
		return err
	}
	return store.headBucket(ctx)
}

// --- Schedule ---

func (s *Service) GetSchedule() (Schedule, error) {
	raw := s.storage.GetSetting(keySchedule, "")
	if raw == "" {
		return Schedule{CronExpr: "0 3 * * *", RetainDays: 14, RetainCount: 30}, nil
	}
	var sch Schedule
	return sch, json.Unmarshal([]byte(raw), &sch)
}

func (s *Service) SetSchedule(sch Schedule) error {
	if sch.Enabled && sch.CronExpr != "" {
		if err := s.applyCron(sch.CronExpr); err != nil {
			return err
		}
	} else {
		_ = s.applyCron("") // stop cron
	}
	b, err := json.Marshal(sch)
	if err != nil {
		return err
	}
	return s.storage.SetSetting(keySchedule, string(b))
}

// --- Records ---

func (s *Service) listRecords() ([]Record, error) {
	raw := s.storage.GetSetting(keyRecords, "[]")
	var records []Record
	return records, json.Unmarshal([]byte(raw), &records)
}

func (s *Service) saveRecords(records []Record) error {
	// Trim to maxRecords, keeping the most recent
	if len(records) > maxRecords {
		records = records[len(records)-maxRecords:]
	}
	b, err := json.Marshal(records)
	if err != nil {
		return err
	}
	return s.storage.SetSetting(keyRecords, string(b))
}

func (s *Service) ListRecords() ([]Record, error) {
	records, err := s.listRecords()
	if err != nil {
		return nil, err
	}
	// Return newest first
	for i, j := 0, len(records)-1; i < j; i, j = i+1, j-1 {
		records[i], records[j] = records[j], records[i]
	}
	return records, nil
}

func (s *Service) upsertRecord(rec Record) error {
	records, err := s.listRecords()
	if err != nil {
		records = nil
	}
	found := false
	for i, r := range records {
		if r.ID == rec.ID {
			records[i] = rec
			found = true
			break
		}
	}
	if !found {
		records = append(records, rec)
	}
	return s.saveRecords(records)
}

func (s *Service) DeleteRecord(ctx context.Context, id string) error {
	records, err := s.listRecords()
	if err != nil {
		return err
	}
	var rec *Record
	for i, r := range records {
		if r.ID == id {
			rec = &records[i]
			break
		}
	}
	if rec == nil {
		return fmt.Errorf("backup record not found")
	}
	// Delete from S3 if possible
	if rec.S3Key != "" {
		cfg, ok, err := s.GetS3Config()
		if err == nil && ok {
			store, err := newS3Store(ctx, cfg)
			if err == nil {
				_ = store.delete(ctx, rec.S3Key)
			}
		}
	}
	filtered := records[:0]
	for _, r := range records {
		if r.ID != id {
			filtered = append(filtered, r)
		}
	}
	return s.saveRecords(filtered)
}

func (s *Service) GetDownloadURL(ctx context.Context, id string) (string, error) {
	records, err := s.listRecords()
	if err != nil {
		return "", err
	}
	var rec *Record
	for _, r := range records {
		if r.ID == id {
			rec = &r
			break
		}
	}
	if rec == nil {
		return "", fmt.Errorf("backup record not found")
	}
	cfg, ok, err := s.GetS3Config()
	if err != nil || !ok {
		return "", fmt.Errorf("S3 not configured")
	}
	store, err := newS3Store(ctx, cfg)
	if err != nil {
		return "", err
	}
	return store.presignURL(ctx, rec.S3Key, presignExpiry)
}

// --- Backup execution ---

func (s *Service) StartBackup(ctx context.Context, triggeredBy string) (*Record, error) {
	s.mu.Lock()
	if s.running {
		s.mu.Unlock()
		return nil, fmt.Errorf("a backup is already in progress")
	}
	s.running = true
	s.mu.Unlock()

	cfg, ok, err := s.GetS3Config()
	if err != nil || !ok {
		s.mu.Lock()
		s.running = false
		s.mu.Unlock()
		return nil, fmt.Errorf("S3 not configured")
	}

	id := uuid.NewString()
	stamp := time.Now().UTC().Format("20060102_150405")
	fileName := fmt.Sprintf("muxapi_%s.sql.gz", stamp)
	s3Key := cfg.Prefix
	if s3Key != "" && s3Key[len(s3Key)-1] != '/' {
		s3Key += "/"
	}
	s3Key += fileName

	rec := Record{
		ID: id, Status: "running", FileName: fileName, S3Key: s3Key,
		TriggeredBy: triggeredBy, StartedAt: time.Now().Unix(),
	}
	_ = s.upsertRecord(rec)

	go func() {
		defer func() {
			s.mu.Lock()
			s.running = false
			s.mu.Unlock()
		}()
		size, err := s.runBackup(context.Background(), cfg, s3Key)
		rec.FinishedAt = time.Now().Unix()
		if err != nil {
			rec.Status = "failed"
			rec.Error = err.Error()
			slog.Warn("backup: run failed", "id", id, "err", err)
		} else {
			rec.Status = "completed"
			rec.SizeBytes = size
		}
		_ = s.upsertRecord(rec)
		_ = s.pruneOldBackups(context.Background())
	}()

	return &rec, nil
}

func (s *Service) runBackup(ctx context.Context, cfg S3Config, s3Key string) (int64, error) {
	r, err := Dump(ctx, s.dbURL)
	if err != nil {
		return 0, fmt.Errorf("pg_dump: %w", err)
	}

	pr, pw := io.Pipe()
	go func() {
		gz := gzip.NewWriter(pw)
		_, copyErr := io.Copy(gz, r)
		if closeErr := r.Close(); closeErr != nil && copyErr == nil {
			copyErr = closeErr
		}
		_ = gz.Close()
		pw.CloseWithError(copyErr)
	}()

	store, err := newS3Store(ctx, cfg)
	if err != nil {
		_ = pr.Close()
		return 0, err
	}
	return store.upload(ctx, s3Key, pr)
}

func (s *Service) pruneOldBackups(ctx context.Context) error {
	sch, err := s.GetSchedule()
	if err != nil {
		return err
	}
	cfg, ok, err := s.GetS3Config()
	if err != nil || !ok {
		return err
	}
	store, err := newS3Store(ctx, cfg)
	if err != nil {
		return err
	}
	records, err := s.listRecords()
	if err != nil {
		return err
	}
	now := time.Now()
	var keep []Record
	for _, r := range records {
		if r.Status != "completed" {
			keep = append(keep, r)
			continue
		}
		tooOld := sch.RetainDays > 0 && now.Sub(time.Unix(r.StartedAt, 0)) > time.Duration(sch.RetainDays)*24*time.Hour
		if tooOld {
			if r.S3Key != "" {
				_ = store.delete(ctx, r.S3Key)
			}
			continue
		}
		keep = append(keep, r)
	}
	// Trim by count (keep newest)
	if sch.RetainCount > 0 {
		completed := 0
		var filtered []Record
		for i := len(keep) - 1; i >= 0; i-- {
			if keep[i].Status == "completed" {
				completed++
				if completed > sch.RetainCount {
					if keep[i].S3Key != "" {
						_ = store.delete(ctx, keep[i].S3Key)
					}
					continue
				}
			}
			filtered = append([]Record{keep[i]}, filtered...)
		}
		keep = filtered
	}
	return s.saveRecords(keep)
}
