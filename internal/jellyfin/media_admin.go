package jellyfin

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/adhikariprivate/rcfin-server/internal/database/model"
	"github.com/adhikariprivate/rcfin-server/internal/organization"
)

type mediaOperationRequest struct {
	CollectionID string `json:"collection_id"`
	ItemKey      string `json:"item_key"`
	Action       string `json:"action"`
}

type mediaOperationsPage struct {
	Operations []model.MediaOperation `json:"operations"`
	NextCursor string                 `json:"next_cursor,omitempty"`
}

func (j *Jellyfin) mediaOperationsGetHandler(w http.ResponseWriter, r *http.Request) {
	if !j.requireMediaAdmin(w, r) {
		return
	}
	collectionID := strings.TrimSpace(r.URL.Query().Get("collection_id"))
	if collectionID == "" || j.collections.GetCollection(collectionID) == nil {
		apierror(w, "a configured collection_id is required", http.StatusBadRequest)
		return
	}
	limit := 100
	if value := r.URL.Query().Get("limit"); value != "" {
		parsed, err := strconv.Atoi(value)
		if err != nil || parsed < 1 || parsed > 1000 {
			apierror(w, "limit must be between 1 and 1000", http.StatusBadRequest)
			return
		}
		limit = parsed
	}
	beforeUpdatedAt, beforeID, err := decodeMediaOperationCursor(r.URL.Query().Get("cursor"))
	if err != nil {
		apierror(w, "cursor is invalid", http.StatusBadRequest)
		return
	}
	operations, err := j.repo.ListMediaOperationsPage(r.Context(), collectionID, limit, beforeUpdatedAt, beforeID)
	if err != nil {
		apierror(w, "unable to read media operation status", http.StatusInternalServerError)
		return
	}
	page := mediaOperationsPage{Operations: operations}
	if len(operations) == limit {
		page.NextCursor = encodeMediaOperationCursor(operations[len(operations)-1])
	}
	serveJSON(page, w)
}

func (j *Jellyfin) mediaOperationRetryHandler(w http.ResponseWriter, r *http.Request) {
	if !j.requireMediaAdmin(w, r) {
		return
	}
	var request mediaOperationRequest
	if !decodeMediaOperationRequest(w, r, &request) {
		return
	}
	if request.ItemKey == "" || request.Action == "" {
		apierror(w, "item_key and action are required", http.StatusBadRequest)
		return
	}
	if !j.validMediaOperationCollection(w, request.CollectionID) {
		return
	}
	operation, err := j.repo.GetMediaOperation(r.Context(), request.CollectionID, request.ItemKey, request.Action)
	if err != nil {
		apierror(w, "unable to read media operation", http.StatusInternalServerError)
		return
	}
	if operation == nil {
		apierror(w, "media operation not found", http.StatusNotFound)
		return
	}
	if operation.Status == "needs_review" || operation.Status == "conflict" {
		apierror(w, "operation needs review; resolve its identity or conflict before retrying", http.StatusConflict)
		return
	}
	switch operation.Status {
	case "failed_retryable", "failed_terminal", "failed", "retryable":
	default:
		apierror(w, "operation is not in a retryable state", http.StatusConflict)
		return
	}
	queueMediaOperation(operation)
	if err := j.repo.UpsertMediaOperation(r.Context(), operation); err != nil {
		apierror(w, "unable to queue media operation retry", http.StatusInternalServerError)
		return
	}
	w.Header().Set("content-type", "application/json")
	w.WriteHeader(http.StatusAccepted)
	serveJSON(operation, w)
}

func (j *Jellyfin) mediaOperationsRefreshHandler(w http.ResponseWriter, r *http.Request) {
	if !j.requireMediaAdmin(w, r) {
		return
	}
	var request mediaOperationRequest
	if !decodeMediaOperationRequest(w, r, &request) {
		return
	}
	if request.CollectionID == "" {
		apierror(w, "collection_id is required", http.StatusBadRequest)
		return
	}
	if !j.validMediaOperationCollection(w, request.CollectionID) {
		return
	}
	queued := 0
	var beforeUpdatedAt time.Time
	var beforeID string
	const pageSize = 500
	for {
		operations, err := j.repo.ListMediaOperationsPage(r.Context(), request.CollectionID, pageSize, beforeUpdatedAt, beforeID)
		if err != nil {
			apierror(w, "unable to read media operation status", http.StatusInternalServerError)
			return
		}
		if len(operations) == 0 {
			break
		}
		last := operations[len(operations)-1]
		beforeUpdatedAt, beforeID = last.UpdatedAt, last.ID
		for i := range operations {
			operation := &operations[i]
			if request.ItemKey != "" && operation.ItemKey != request.ItemKey {
				continue
			}
			refreshable := operation.Action == "metadata_refresh" || strings.HasPrefix(operation.Action, "subtitle_refresh:")
			if !refreshable || operation.Status != "complete" {
				continue
			}
			queueMediaOperation(operation)
			if err := j.repo.UpsertMediaOperation(r.Context(), operation); err != nil {
				apierror(w, "unable to queue media refresh", http.StatusInternalServerError)
				return
			}
			queued++
		}
		if len(operations) < pageSize {
			break
		}
	}
	if queued == 0 {
		apierror(w, "no completed metadata or subtitle operations matched", http.StatusNotFound)
		return
	}
	w.Header().Set("content-type", "application/json")
	w.WriteHeader(http.StatusAccepted)
	serveJSON(map[string]int{"queued": queued}, w)
}

func (j *Jellyfin) mediaOperationStageHandler(w http.ResponseWriter, r *http.Request) {
	if !j.requireMediaAdmin(w, r) {
		return
	}
	var request mediaOperationRequest
	if !decodeMediaOperationRequest(w, r, &request) {
		return
	}
	if request.ItemKey == "" {
		apierror(w, "item_key is required", http.StatusBadRequest)
		return
	}
	if !j.validMediaOperationCollection(w, request.CollectionID) {
		return
	}
	coll := j.collections.GetCollection(request.CollectionID)
	operation, err := organization.QueueExistingItemForInbox(r.Context(), coll, j.repo, request.ItemKey)
	if err != nil {
		apierror(w, "Inbox staging request could not be queued; verify the source, destination, and library move settings", http.StatusConflict)
		return
	}
	w.Header().Set("content-type", "application/json")
	w.WriteHeader(http.StatusAccepted)
	serveJSON(operation, w)
}

func (j *Jellyfin) requireMediaAdmin(w http.ResponseWriter, r *http.Request) bool {
	request := j.getRequestCtx(w, r)
	if request == nil {
		return false
	}
	if request.User == nil || !request.User.Properties.Admin {
		apierror(w, "administrator access required", http.StatusForbidden)
		return false
	}
	return true
}

func (j *Jellyfin) validMediaOperationCollection(w http.ResponseWriter, collectionID string) bool {
	if strings.TrimSpace(collectionID) == "" || j.collections.GetCollection(collectionID) == nil {
		apierror(w, "configured collection_id is required", http.StatusBadRequest)
		return false
	}
	return true
}

func decodeMediaOperationRequest(w http.ResponseWriter, r *http.Request, request *mediaOperationRequest) bool {
	r.Body = http.MaxBytesReader(w, r.Body, 16*1024)
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(request); err != nil {
		apierror(w, "invalid media operation request", http.StatusBadRequest)
		return false
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		apierror(w, "request must contain a single JSON object", http.StatusBadRequest)
		return false
	}
	request.CollectionID = strings.TrimSpace(request.CollectionID)
	request.ItemKey = strings.TrimSpace(request.ItemKey)
	request.Action = strings.TrimSpace(request.Action)
	return true
}

func queueMediaOperation(operation *model.MediaOperation) {
	now := time.Now().UTC()
	operation.Status = "planned"
	operation.NextAttempt = now
	operation.LeaseOwner = ""
	operation.LeaseUntil = now
	operation.Error = ""
	operation.UpdatedAt = now
}

func encodeMediaOperationCursor(operation model.MediaOperation) string {
	value := operation.UpdatedAt.UTC().Format(time.RFC3339Nano) + "\n" + operation.ID
	return base64.RawURLEncoding.EncodeToString([]byte(value))
}

func decodeMediaOperationCursor(cursor string) (time.Time, string, error) {
	if cursor == "" {
		return time.Time{}, "", nil
	}
	if len(cursor) > 4096 {
		return time.Time{}, "", errors.New("cursor is too long")
	}
	decoded, err := base64.RawURLEncoding.DecodeString(cursor)
	if err != nil {
		return time.Time{}, "", err
	}
	parts := strings.SplitN(string(decoded), "\n", 2)
	if len(parts) != 2 || parts[1] == "" {
		return time.Time{}, "", errors.New("malformed cursor")
	}
	updatedAt, err := time.Parse(time.RFC3339Nano, parts[0])
	if err != nil {
		return time.Time{}, "", err
	}
	return updatedAt, parts[1], nil
}
