package store

import (
	"database/sql"
	"strings"
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
	Stream       bool   `json:"stream"`       // 探测是否走流式（请求体加 stream:true）
	ProbeText    string `json:"probe_text"`   // 探测消息内容，空=默认 "hi"
	MaxTokens    int    `json:"max_tokens"`   // 探测 max_tokens，0=默认 1
	IntervalSec  int    `json:"interval_sec"` // 该项探测周期(秒)，0=用全局
	Path         string `json:"path"`         // 该项探测端点，空=用全局
	UpstreamName string `json:"upstream_name,omitempty"`
	BaseURL      string `json:"-"`
	APIKey       string `json:"-"`
	Proxy        string `json:"-"`
	ChannelProbe bool   `json:"-"` // 兼容旧数据；运行时不再改变熔断粒度
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
	rows, err := s.db.Query(q + ` ORDER BY m.sort, m.id`)
	if err != nil {
		return nil, err
	}
	return scanMonitors(rows)
}

func (s *Store) GetMonitor(id int64) (*Monitor, error) {
	rows, err := s.db.Query(monitorJoin+` WHERE m.id=?`, id)
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
	var id int64
	err := s.db.QueryRow(`INSERT INTO monitors(upstream_id,model,name,enabled,stream,probe_text,max_tokens,interval_sec,path,sort)
		VALUES(?,?,?,?,?,?,?,?,?,(SELECT COALESCE(MAX(sort),0)+1 FROM monitors)) RETURNING id`,
		m.UpstreamID, m.Model, m.Name, m.Enabled, m.Stream, m.ProbeText, m.MaxTokens, m.IntervalSec, m.Path).Scan(&id)
	return id, err
}

// ReorderMonitors 按给定 id 顺序写入 sort 权重（从 1 起，下标即权重）。
// 一次事务内全量更新；未出现在 ids 中的监控项保持原 sort，会排在已排项之后或之间。
func (s *Store) ReorderMonitors(ids []int64) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for i, id := range ids {
		if _, err := tx.Exec(`UPDATE monitors SET sort=? WHERE id=?`, i+1, id); err != nil {
			return err
		}
	}
	return tx.Commit()
}
func (s *Store) MonitoredModels(upstreamID int64) (map[string]bool, error) {
	rows, err := s.db.Query(`SELECT model FROM monitors WHERE upstream_id=?`, upstreamID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	set := make(map[string]bool)
	for rows.Next() {
		var m string
		if err := rows.Scan(&m); err == nil {
			set[m] = true
		}
	}
	return set, rows.Err()
}

// BatchCreateMonitors 为某上游的一批模型批量建监控，已存在 (upstream,model) 的跳过。
// tmpl 提供共享探测参数（model 字段被逐个模型覆盖）。返回 (created, skipped)。
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
		m := tmpl // 复制模板
		m.UpstreamID = upstreamID
		m.Model = model
		if _, err := s.CreateMonitor(&m); err != nil {
			return created, skipped, err
		}
		existing[model] = true // 防同批重复
		created++
	}
	return created, skipped, nil
}

func (s *Store) UpdateMonitor(m *Monitor) error {
	_, err := s.db.Exec(`UPDATE monitors SET upstream_id=?,model=?,name=?,enabled=?,stream=?,probe_text=?,max_tokens=?,interval_sec=?,path=? WHERE id=?`,
		m.UpstreamID, m.Model, m.Name, m.Enabled, m.Stream, m.ProbeText, m.MaxTokens, m.IntervalSec, m.Path, m.ID)
	return err
}

func (s *Store) DeleteMonitor(id int64) error {
	_, err := s.db.Exec(`DELETE FROM monitors WHERE id=?`, id)
	return err
}
