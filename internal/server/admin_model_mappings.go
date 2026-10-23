package server

import (
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/mirainya/muxapi/internal/modelmapping"
	"github.com/mirainya/muxapi/internal/store"
)

// modelMappingSvc holds the model mapping service reference for admin use.
var modelMappingSvc *modelmapping.Service

// SetModelMappingService stores the service reference for admin endpoints.
func (s *Server) SetModelMappingService(svc *modelmapping.Service) {
	modelMappingSvc = svc
}

// adminModelMappings handles GET (list) and POST (create/update) for model mappings.
func (s *Server) adminModelMappings(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		s.listModelMappings(w, r)
	case http.MethodPost:
		s.createModelMapping(w, r)
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

// adminModelMappingItem handles PUT (update) and DELETE (remove) for a single mapping.
func (s *Server) adminModelMappingItem(w http.ResponseWriter, r *http.Request) {
	idStr := strings.TrimPrefix(r.URL.Path, "/admin/model-mappings/")
	if idStr == "cleanup" {
		s.cleanupModelMappings(w, r)
		return
	}
	id, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil || id <= 0 {
		http.Error(w, "invalid mapping id", http.StatusBadRequest)
		return
	}
	switch r.Method {
	case http.MethodDelete:
		s.deleteModelMapping(w, r, id)
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func (s *Server) listModelMappings(w http.ResponseWriter, r *http.Request) {
	var upstreamID *int64
	if v := r.URL.Query().Get("upstream_id"); v != "" {
		id, err := strconv.ParseInt(v, 10, 64)
		if err != nil || id < 0 {
			http.Error(w, "invalid upstream id", http.StatusBadRequest)
			return
		}
		upstreamID = &id
	}
	mappings, err := s.store.ListModelMappings(upstreamID)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if mappings == nil {
		mappings = []store.ModelMapping{}
	}
	writeJSON(w, mappings)
}

func (s *Server) createModelMapping(w http.ResponseWriter, r *http.Request) {
	var req struct {
		UpstreamID  int64  `json:"upstream_id"`
		SourceModel string `json:"source_model"`
		TargetModel string `json:"target_model"`
		MappingType string `json:"mapping_type"`
		TTLHours    int    `json:"ttl_hours"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid request body", http.StatusBadRequest)
		return
	}
	if strings.TrimSpace(req.SourceModel) == "" || strings.TrimSpace(req.TargetModel) == "" {
		http.Error(w, "source_model and target_model are required", http.StatusBadRequest)
		return
	}
	mappingType := store.MappingStatic
	if req.MappingType == store.MappingAuto {
		mappingType = store.MappingAuto
	}
	mapping := &store.ModelMapping{
		UpstreamID:  req.UpstreamID,
		SourceModel: strings.TrimSpace(req.SourceModel),
		TargetModel: strings.TrimSpace(req.TargetModel),
		MappingType: mappingType,
	}
	if req.TTLHours > 0 {
		expires := time.Now().Add(time.Duration(req.TTLHours) * time.Hour)
		mapping.ExpiresAt = &expires
	}
	if err := s.store.UpsertModelMapping(mapping); err != nil {
		if errors.Is(err, store.ErrMappingUpstreamMissing) {
			http.Error(w, "upstream no longer exists", http.StatusBadRequest)
			return
		}
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	// Invalidate in-memory cache
	if modelMappingSvc != nil {
		modelMappingSvc.InvalidateAll()
	}
	w.WriteHeader(http.StatusCreated)
	writeJSON(w, mapping)
}

func (s *Server) deleteModelMapping(w http.ResponseWriter, _ *http.Request, id int64) {
	if err := s.store.DeleteModelMapping(id); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			http.Error(w, "mapping not found", http.StatusNotFound)
			return
		}
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if modelMappingSvc != nil {
		modelMappingSvc.InvalidateAll()
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) cleanupModelMappings(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var req struct {
		IDs []int64 `json:"ids"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 32768)).Decode(&req); err != nil || len(req.IDs) == 0 || len(req.IDs) > 1000 {
		http.Error(w, "provide 1 to 1000 mapping ids", http.StatusBadRequest)
		return
	}
	for _, id := range req.IDs {
		if id <= 0 {
			http.Error(w, "invalid mapping id", http.StatusBadRequest)
			return
		}
	}
	deleted, err := s.store.DeleteOrphanModelMappings(req.IDs)
	if err != nil {
		http.Error(w, "mapping cleanup failed", http.StatusInternalServerError)
		return
	}
	if modelMappingSvc != nil {
		modelMappingSvc.InvalidateAll()
	}
	writeJSON(w, map[string]int64{"deleted": deleted})
}
