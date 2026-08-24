package store

import (
	"database/sql"
	"strings"

	"gorm.io/gorm"
)

// Monitor 监控项：对某渠道的某个模型主动探测。凭证用渠道自带的 api_key。
// UpstreamName/BaseURL/APIKey 为 JOIN 出的渠道视图字段（探测/展示用）。
// Stream/ProbeText/MaxTokens/IntervalSec/Path 为可配探测参数，
// 空字符串/0 一律表示「沿用全局默认」（见 monitor.Prober）。
type Monitor struct {
	ID           int64  `json:"id"`
	UpstreamID   int64  `json:"upstream_id"`
	Model        string `json:"model"`
	Name         string `json:"name"`
	Enabled      bool   `json:"enabled"`
	Stream       bool   `json:"stream"`
	ProbeText    string `json:"probe_text"`
	MaxTokens    int    `json:"max_tokens"`
	IntervalSec  int    `json:"interval_sec"`
	Path         string `json:"path"`
	UpstreamName string `json:"upstream_name,omitempty"`
	BaseURL      string `json:"-"`
	APIKey       string `json:"-"`
	Proxy        string `json:"-"`
	ChannelProbe bool   `json:"-"`
}

// --- 监控项 ---

// scanMonitors 扫描含渠道视图(name/base_url/api_key)的 JOIN 行。
func scanMonitors(rows *sql.Rows) ([]*Monitor, error) {
	defer rows.Close()
	var ms []*Monitor
	for rows.Next() {
		m := &Monitor{}
		if err := rows.Scan(&m.ID, &m.UpstreamID, &m.Model, &m.Name, &m.Enabled,
			&m.Stream, &m.ProbeText, &m.MaxTokens, &m.IntervalSec, &m.Path,
			&m.UpstreamName, &m.BaseURL, &m.APIKey, &m.Proxy, &m.ChannelProbe); err != nil {
			return nil, err
		}
		ms = append(ms, m)
	}
	return ms, rows.Err()
}

const monitorJoin = `SELECT m.id,m.upstream_id,m.model,m.name,m.enabled,m.stream,m.probe_text,m.max_tokens,m.interval_sec,m.path,u.name,u.base_url,u.api_key,u.proxy,u.channel_probe
	FROM monitors m JOIN upstreams u ON u.id=m.upstream_id`

// ListMonitors 全部监控项；enabledOnly 时只返回启用且渠道也启用的（探测用）。
func (s *Store) ListMonitors(enabledOnly bool) ([]*Monitor, error) {
	q := monitorJoin
	if enabledOnly {
		q += ` WHERE m.enabled=TRUE AND u.enabled=TRUE`
	}
	rows, err := s.query(q + ` ORDER BY m.sort, m.id`)
	if err != nil {
		return nil, err
	}
	return scanMonitors(rows)
}

func (s *Store) GetMonitor(id int64) (*Monitor, error) {
	rows, err := s.query(monitorJoin+` WHERE m.id=?`, id)
	if err != nil {
		return nil, err
	}
	ms, err := scanMonitors(rows)
	if err != nil || len(ms) == 0 {
		return nil, err
	}
	return ms[0], nil
}

func (s *Store) CreateMonitor(m *Monitor) (int64, error) {
	// 取当前最大 sort 值
	var maxSort int
	s.gormDB.Model(&MonitorModel{}).Select("COALESCE(MAX(sort),0)").Scan(&maxSort)
	model := MonitorModel{
		UpstreamID:  m.UpstreamID,
		Model:       m.Model,
		Name:        m.Name,
		Enabled:     m.Enabled,
		Stream:      m.Stream,
		ProbeText:   m.ProbeText,
		MaxTokens:   m.MaxTokens,
		IntervalSec: m.IntervalSec,
		Path:        m.Path,
		Sort:        maxSort + 1,
	}
	if err := s.gormDB.Create(&model).Error; err != nil {
		return 0, err
	}
	return model.ID, nil
}

// ReorderMonitors 按给定 id 顺序写入 sort 权重（从 1 起，下标即权重）。
func (s *Store) ReorderMonitors(ids []int64) error {
	return s.gormDB.Transaction(func(tx *gorm.DB) error {
		for i, id := range ids {
			if err := tx.Model(&MonitorModel{}).Where("id = ?", id).Update("sort", i+1).Error; err != nil {
				return err
			}
		}
		return nil
	})
}

func (s *Store) MonitoredModels(upstreamID int64) (map[string]bool, error) {
	var models []string
	if err := s.gormDB.Model(&MonitorModel{}).Where("upstream_id = ?", upstreamID).Pluck("model", &models).Error; err != nil {
		return nil, err
	}
	set := make(map[string]bool, len(models))
	for _, m := range models {
		set[m] = true
	}
	return set, nil
}

// BatchCreateMonitors 为某上游的一批模型批量建监控，已存在 (upstream,model) 的跳过。
func (s *Store) BatchCreateMonitors(upstreamID int64, models []string, tmpl Monitor) (int, int, error) {
	existing, err := s.MonitoredModels(upstreamID)
	if err != nil {
		return 0, 0, err
	}
	created, skipped := 0, 0
	for _, model := range models {
		model = strings.TrimSpace(model)
		if model == "" || existing[model] {
			skipped++
			continue
		}
		m := tmpl
		m.UpstreamID = upstreamID
		m.Model = model
		if _, err := s.CreateMonitor(&m); err != nil {
			return created, skipped, err
		}
		existing[model] = true
		created++
	}
	return created, skipped, nil
}

func (s *Store) UpdateMonitor(m *Monitor) error {
	return s.gormDB.Model(&MonitorModel{}).Where("id = ?", m.ID).Updates(map[string]any{
		"upstream_id":  m.UpstreamID,
		"model":        m.Model,
		"name":         m.Name,
		"enabled":      m.Enabled,
		"stream":       m.Stream,
		"probe_text":   m.ProbeText,
		"max_tokens":   m.MaxTokens,
		"interval_sec": m.IntervalSec,
		"path":         m.Path,
	}).Error
}

func (s *Store) DeleteMonitor(id int64) error {
	return s.gormDB.Delete(&MonitorModel{}, id).Error
}
