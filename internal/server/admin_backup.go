package server

import (
	"encoding/json"
	"net/http"
	"strings"

	"github.com/mirainya/muxapi/internal/backup"
)

// adminBackup routes /admin/backup and /admin/backup/*
func (s *Server) adminBackup(w http.ResponseWriter, r *http.Request) {
	rest := strings.TrimPrefix(r.URL.Path, "/admin/backup")
	rest = strings.TrimPrefix(rest, "/")

	switch {
	case rest == "" || rest == "/":
		switch r.Method {
		case http.MethodGet:
			s.listBackups(w, r)
		case http.MethodPost:
			s.triggerBackup(w, r)
		default:
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		}

	case rest == "config":
		switch r.Method {
		case http.MethodGet:
			s.getBackupConfig(w, r)
		case http.MethodPut:
			s.setBackupConfig(w, r)
		default:
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		}

	case rest == "config/test":
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		s.testBackupConfig(w, r)

	case rest == "schedule":
		switch r.Method {
		case http.MethodGet:
			s.getBackupSchedule(w, r)
		case http.MethodPut:
			s.setBackupSchedule(w, r)
		default:
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		}

	case strings.HasPrefix(rest, "records/"):
		id := strings.TrimPrefix(rest, "records/")
		id = strings.TrimSuffix(id, "/download")
		if strings.HasSuffix(rest, "/download") {
			if r.Method != http.MethodGet {
				http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
				return
			}
			s.backupDownloadURL(w, r, id)
			return
		}
		if r.Method != http.MethodDelete {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		s.deleteBackup(w, r, id)

	default:
		http.NotFound(w, r)
	}
}

func (s *Server) getBackupConfig(w http.ResponseWriter, r *http.Request) {
	if s.backupSvc == nil {
		http.Error(w, "backup service unavailable", http.StatusServiceUnavailable)
		return
	}
	cfg, ok, err := s.backupSvc.GetS3Config()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if !ok {
		writeJSON(w, backup.S3Config{})
		return
	}
	// Mask secret key: return placeholder so frontend knows it's set
	masked := cfg
	if masked.SecretKey != "" {
		masked.SecretKey = "••••••••"
	}
	writeJSON(w, masked)
}

func (s *Server) setBackupConfig(w http.ResponseWriter, r *http.Request) {
	if s.backupSvc == nil {
		http.Error(w, "backup service unavailable", http.StatusServiceUnavailable)
		return
	}
	var input backup.S3Config
	if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
		http.Error(w, "bad request: "+err.Error(), http.StatusBadRequest)
		return
	}
	// If secret key is the placeholder, keep the existing key
	if input.SecretKey == "••••••••" {
		existing, ok, err := s.backupSvc.GetS3Config()
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		if ok {
			input.SecretKey = existing.SecretKey
		}
	}
	if err := s.backupSvc.SetS3Config(input); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) testBackupConfig(w http.ResponseWriter, r *http.Request) {
	if s.backupSvc == nil {
		http.Error(w, "backup service unavailable", http.StatusServiceUnavailable)
		return
	}
	var input backup.S3Config
	if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
		http.Error(w, "bad request: "+err.Error(), http.StatusBadRequest)
		return
	}
	if input.SecretKey == "••••••••" {
		existing, ok, err := s.backupSvc.GetS3Config()
		if err == nil && ok {
			input.SecretKey = existing.SecretKey
		}
	}
	err := s.backupSvc.TestConnection(r.Context(), input)
	if err != nil {
		writeJSON(w, map[string]any{"ok": false, "message": err.Error()})
		return
	}
	writeJSON(w, map[string]any{"ok": true, "message": "连接成功"})
}

func (s *Server) getBackupSchedule(w http.ResponseWriter, r *http.Request) {
	if s.backupSvc == nil {
		http.Error(w, "backup service unavailable", http.StatusServiceUnavailable)
		return
	}
	sch, err := s.backupSvc.GetSchedule()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, sch)
}

func (s *Server) setBackupSchedule(w http.ResponseWriter, r *http.Request) {
	if s.backupSvc == nil {
		http.Error(w, "backup service unavailable", http.StatusServiceUnavailable)
		return
	}
	var input backup.Schedule
	if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
		http.Error(w, "bad request: "+err.Error(), http.StatusBadRequest)
		return
	}
	if err := s.backupSvc.SetSchedule(input); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) triggerBackup(w http.ResponseWriter, r *http.Request) {
	if s.backupSvc == nil {
		http.Error(w, "backup service unavailable", http.StatusServiceUnavailable)
		return
	}
	rec, err := s.backupSvc.StartBackup(r.Context(), "manual")
	if err != nil {
		http.Error(w, err.Error(), http.StatusConflict)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusAccepted)
	json.NewEncoder(w).Encode(rec)
}

func (s *Server) listBackups(w http.ResponseWriter, r *http.Request) {
	if s.backupSvc == nil {
		http.Error(w, "backup service unavailable", http.StatusServiceUnavailable)
		return
	}
	records, err := s.backupSvc.ListRecords()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if records == nil {
		records = []backup.Record{}
	}
	writeJSON(w, map[string]any{"items": records})
}

func (s *Server) deleteBackup(w http.ResponseWriter, r *http.Request, id string) {
	if s.backupSvc == nil {
		http.Error(w, "backup service unavailable", http.StatusServiceUnavailable)
		return
	}
	if err := s.backupSvc.DeleteRecord(r.Context(), id); err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) backupDownloadURL(w http.ResponseWriter, r *http.Request, id string) {
	if s.backupSvc == nil {
		http.Error(w, "backup service unavailable", http.StatusServiceUnavailable)
		return
	}
	url, err := s.backupSvc.GetDownloadURL(r.Context(), id)
	if err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	writeJSON(w, map[string]string{"url": url})
}
