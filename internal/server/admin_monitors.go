package server

import (
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/mirainya/muxapi/internal/monitor"
	"github.com/mirainya/muxapi/internal/store"
	"github.com/mirainya/muxapi/internal/translate"
	"github.com/mirainya/muxapi/internal/upstream"
)

type monitorDTO struct {
	ID           int64            `json:"id"`
	UpstreamID   int64            `json:"upstream_id"`
	UpstreamName string           `json:"upstream_name"`
	Model        string           `json:"model"`
	Name         string           `json:"name"`
	Enabled      bool             `json:"enabled"`
	Stream       bool             `json:"stream"`
	ProbeText    string           `json:"probe_text"`
	MaxTokens    int              `json:"max_tokens"`
	IntervalSec  int              `json:"interval_sec"`
	Path         string           `json:"path"`
	Snapshot     monitor.Snapshot `json:"snapshot"`
}

// monitorInput 新增/编辑入参。
type monitorInput struct {
	UpstreamID  int64  `json:"upstream_id"`
	Model       string `json:"model"`
	Name        string `json:"name"`
	Enabled     bool   `json:"enabled"`
	Stream      bool   `json:"stream"`
	ProbeText   string `json:"probe_text"`
	MaxTokens   int    `json:"max_tokens"`
	IntervalSec int    `json:"interval_sec"`
	Path        string `json:"path"`
}

func (s *Server) adminMonitors(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		ms, err := s.store.ListMonitors(false)
		if err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
		out := make([]monitorDTO, 0, len(ms))
		for _, m := range ms {
			out = append(out, monitorDTO{
				ID: m.ID, UpstreamID: m.UpstreamID, UpstreamName: m.UpstreamName,
				Model: m.Model, Name: m.Name, Enabled: m.Enabled,
				Stream: m.Stream, ProbeText: m.ProbeText, MaxTokens: m.MaxTokens,
				IntervalSec: m.IntervalSec, Path: m.Path,
				Snapshot: s.mon.Snapshot(m.ID),
			})
		}
		writeJSON(w, out)
	case http.MethodPost:
		in, err := decodeMonitor(r)
		if err != nil {
			http.Error(w, err.Error(), 400)
			return
		}
		// 校验 upstream 存在：避免建出孤儿监控行(被 INNER JOIN 过滤但 POST 误返回成功)
		if _, err := s.store.Get(in.UpstreamID); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				http.Error(w, "upstream not found", 404)
				return
			}
			http.Error(w, err.Error(), 500)
			return
		}
		id, err := s.store.CreateMonitor(in)
		if err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
		writeJSON(w, map[string]int64{"id": id})
	default:
		http.Error(w, "method not allowed", 405)
	}
}

// reorderUpstreams 持久化拖拽后的上游顺序。body: {ids:[3,1,2,...]}
func (s *Server) reorderUpstreams(w http.ResponseWriter, r *http.Request) {
	var in struct {
		IDs []int64 `json:"ids"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		http.Error(w, err.Error(), 400)
		return
	}
	if err := s.store.ReorderUpstreams(in.IDs); err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	w.WriteHeader(204)
}

func (s *Server) adminMonitorItem(w http.ResponseWriter, r *http.Request) {
	rest := strings.TrimPrefix(r.URL.Path, "/admin/monitors/")
	parts := strings.Split(rest, "/")
	if parts[0] == "reorder" && r.Method == http.MethodPost { // 拖拽保存顺序
		s.reorderMonitors(w, r)
		return
	}
	id, err := strconv.ParseInt(parts[0], 10, 64)
	if err != nil {
		http.Error(w, "bad id", 400)
		return
	}
	if len(parts) == 2 && parts[1] == "probe" { // 立即探测一次并返回最新快照
		s.probeMonitorNow(w, r, id)
		return
	}
	switch r.Method {
	case http.MethodPut:
		in, err := decodeMonitor(r)
		if err != nil {
			http.Error(w, err.Error(), 400)
			return
		}
		in.ID = id
		if err := s.store.UpdateMonitor(in); err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
		w.WriteHeader(204)
	case http.MethodDelete:
		if err := s.store.DeleteMonitor(id); err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
		s.mon.Forget(id)
		w.WriteHeader(204)
	default:
		http.Error(w, "method not allowed", 405)
	}
}

// reorderMonitors 持久化拖拽后的卡片顺序。body: {ids:[3,1,2,...]}
func (s *Server) reorderMonitors(w http.ResponseWriter, r *http.Request) {
	var in struct {
		IDs []int64 `json:"ids"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		http.Error(w, err.Error(), 400)
		return
	}
	if err := s.store.ReorderMonitors(in.IDs); err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	w.WriteHeader(204)
}

// probeMonitorNow 立即同步探测该监控项一次，返回最新快照。
func (s *Server) probeMonitorNow(w http.ResponseWriter, r *http.Request, id int64) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", 405)
		return
	}
	m, err := s.store.GetMonitor(id)
	if err != nil || m == nil {
		http.Error(w, "monitor not found", 404)
		return
	}
	s.monProber.Probe(r.Context(), m)
	writeJSON(w, s.mon.Snapshot(id))
}

// decodeMonitor 校验并规范化监控项输入，负数配置恢复为默认值。
func decodeMonitor(r *http.Request) (*store.Monitor, error) {
	var in monitorInput
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		return nil, err
	}
	if in.UpstreamID <= 0 || strings.TrimSpace(in.Model) == "" {
		return nil, errBadMonitor
	}
	if in.MaxTokens < 0 {
		in.MaxTokens = 0
	}
	if in.IntervalSec < 0 {
		in.IntervalSec = 0
	}
	return &store.Monitor{
		UpstreamID: in.UpstreamID, Model: strings.TrimSpace(in.Model),
		Name: strings.TrimSpace(in.Name), Enabled: in.Enabled,
		Stream: in.Stream, ProbeText: in.ProbeText, MaxTokens: in.MaxTokens,
		IntervalSec: in.IntervalSec, Path: strings.TrimSpace(in.Path),
	}, nil
}

type upstreamInput struct {
	Name         string   `json:"name"`
	Source       string   `json:"source"`
	BaseURL      string   `json:"base_url"`
	Proxy        string   `json:"proxy"`
	Protocol     string   `json:"protocol"`
	APIKey       string   `json:"api_key"`
	Enabled      bool     `json:"enabled"`
	ChannelProbe bool     `json:"channel_probe"`
	PrimaryTagID *int64   `json:"primary_tag_id"`
	TagIDs       *[]int64 `json:"tag_ids"`
}

// decodeUpstream 在系统边界校验 URL、协议和标签字段。
func decodeUpstream(r *http.Request) (*upstream.Upstream, error) {
	var d upstreamInput
	if err := json.NewDecoder(r.Body).Decode(&d); err != nil {
		return nil, err
	}
	d.Name = strings.TrimSpace(d.Name)
	d.Source = strings.TrimSpace(d.Source)
	d.BaseURL = strings.TrimSpace(d.BaseURL)
	d.Protocol = strings.TrimSpace(d.Protocol)
	if d.Name == "" {
		return nil, errors.New("name is required")
	}
	// BaseURL 必须是 http/https 绝对 URL（系统边界严格校验）
	u, err := url.Parse(d.BaseURL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return nil, errors.New("base_url must be a valid http(s) URL")
	}
	protocol, ok := translate.NormalizeFormat(d.Protocol)
	if !ok {
		return nil, errors.New("unsupported upstream protocol")
	}
	result := &upstream.Upstream{
		Name: d.Name, Source: d.Source, BaseURL: d.BaseURL, APIKey: d.APIKey, Proxy: d.Proxy, Enabled: d.Enabled,
		Protocol: string(protocol), ChannelProbe: d.ChannelProbe,
	}
	if d.PrimaryTagID != nil || d.TagIDs != nil {
		result.TagsSet = true
		if d.PrimaryTagID != nil && *d.PrimaryTagID > 0 {
			result.PrimaryTagID = *d.PrimaryTagID
		}
		if d.TagIDs != nil {
			result.TagIDs = *d.TagIDs
		}
	}
	return result, nil
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(v)
}
