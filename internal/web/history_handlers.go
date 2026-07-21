package web

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"gpuardian/internal/history"
)

type historyResultRequest struct {
	Outcome   string             `json:"outcome"`
	Note      string             `json:"note"`
	Artifacts []history.Artifact `json:"artifacts"`
	Version   int                `json:"version"`
}

type historySearchRequest struct {
	Filter history.SearchExpression `json:"filter"`
	Sort   history.SearchSort       `json:"sort"`
	Limit  int                      `json:"limit"`
	Cursor string                   `json:"cursor"`
}

func (s *Server) handleHistorySearch(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSONError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	if s.History == nil {
		writeJSONError(w, http.StatusServiceUnavailable, "history is unavailable")
		return
	}
	var request historySearchRequest
	if err := decodeJSONBody(r, &request); err != nil {
		writeJSONError(w, http.StatusBadRequest, err.Error())
		return
	}
	if request.Limit == 0 {
		request.Limit = 50
	}
	if request.Limit < 1 || request.Limit > 100 {
		writeJSONError(w, http.StatusBadRequest, "limit must be between 1 and 100")
		return
	}
	cursor, ok := decodeHistorySearchCursor(request.Cursor)
	if request.Cursor != "" && !ok {
		writeJSONError(w, http.StatusBadRequest, "invalid cursor")
		return
	}
	summary, sessions, nextCursor, err := s.History.Search(r.Context(), request.Filter, request.Sort, request.Limit, cursor)
	if errors.Is(err, history.ErrInvalidSearchFilter) {
		writeJSONError(w, http.StatusBadRequest, err.Error())
		return
	}
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	next := ""
	if len(sessions) == request.Limit && len(sessions) > 0 {
		next = encodeHistorySearchCursor(nextCursor)
	}
	writeJSON(w, http.StatusOK, map[string]any{"summary": summary, "sessions": sessions, "next_cursor": next})
}

func (s *Server) handleHistorySummary(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeJSONError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	if s.History == nil {
		writeJSONError(w, http.StatusServiceUnavailable, "history is unavailable")
		return
	}
	filter, err := parseHistoryFilter(r)
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, err.Error())
		return
	}
	summary, err := s.History.Summary(r.Context(), filter)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, summary)
}

func (s *Server) handleHistorySessions(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeJSONError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	if s.History == nil {
		writeJSONError(w, http.StatusServiceUnavailable, "history is unavailable")
		return
	}
	filter, err := parseHistoryFilter(r)
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, err.Error())
		return
	}
	sessions, err := s.History.ListSessions(r.Context(), filter)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	next := ""
	if len(sessions) == filter.Limit && len(sessions) > 0 {
		last := sessions[len(sessions)-1]
		next = encodeHistoryCursor(last.StartsAt.UnixMilli(), last.ID)
	}
	writeJSON(w, http.StatusOK, map[string]any{"sessions": sessions, "next_cursor": next})
}

func (s *Server) handleHistorySessionAction(w http.ResponseWriter, r *http.Request) {
	if s.History == nil {
		writeJSONError(w, http.StatusServiceUnavailable, "history is unavailable")
		return
	}
	rest := strings.TrimPrefix(r.URL.Path, "/api/history/sessions/")
	parts := strings.Split(strings.Trim(rest, "/"), "/")
	if len(parts) == 0 || parts[0] == "" || len(parts) > 2 {
		writeJSONError(w, http.StatusNotFound, "not found")
		return
	}
	id := parts[0]
	if len(parts) == 1 && r.Method == http.MethodGet {
		session, err := s.History.GetSession(r.Context(), id)
		if errors.Is(err, history.ErrNotFound) {
			writeJSONError(w, http.StatusNotFound, err.Error())
			return
		}
		if err != nil {
			writeJSONError(w, http.StatusInternalServerError, err.Error())
			return
		}
		writeJSON(w, http.StatusOK, session)
		return
	}
	if len(parts) == 2 && parts[1] == "jobs" && r.Method == http.MethodGet {
		limit := parseBoundedInt(r.URL.Query().Get("limit"), 100, 1, 100)
		after, ok := decodeHistoryCursor(r.URL.Query().Get("cursor"))
		if r.URL.Query().Get("cursor") != "" && !ok {
			writeJSONError(w, http.StatusBadRequest, "invalid cursor")
			return
		}
		jobs, err := s.History.ListJobs(r.Context(), id, limit, after.At, after.ID)
		if err != nil {
			writeJSONError(w, http.StatusInternalServerError, err.Error())
			return
		}
		next := ""
		if len(jobs) == limit && len(jobs) > 0 && jobs[len(jobs)-1].StartedAt != nil {
			last := jobs[len(jobs)-1]
			next = encodeHistoryCursor(last.StartedAt.UnixMilli(), last.CursorID)
		}
		writeJSON(w, http.StatusOK, map[string]any{"jobs": jobs, "next_cursor": next})
		return
	}
	if len(parts) == 2 && parts[1] == "result" && r.Method == http.MethodPut {
		var request historyResultRequest
		if err := decodeJSONBody(r, &request); err != nil {
			writeJSONError(w, http.StatusBadRequest, err.Error())
			return
		}
		request.Outcome = strings.TrimSpace(request.Outcome)
		request.Note = strings.TrimSpace(request.Note)
		if err := history.ValidateResult(request.Outcome, request.Note, request.Artifacts); err != nil {
			writeJSONError(w, http.StatusBadRequest, err.Error())
			return
		}
		current, _ := currentSession(r)
		result, err := s.History.PutResult(r.Context(), id, current.User, request.Outcome, request.Note, request.Artifacts, request.Version)
		switch {
		case errors.Is(err, history.ErrNotFound):
			writeJSONError(w, http.StatusNotFound, err.Error())
		case errors.Is(err, history.ErrForbidden):
			writeJSONError(w, http.StatusForbidden, err.Error())
		case errors.Is(err, history.ErrVersionConflict):
			writeJSONError(w, http.StatusConflict, err.Error())
		case err != nil:
			writeJSONError(w, http.StatusInternalServerError, err.Error())
		default:
			writeJSON(w, http.StatusOK, result)
		}
		return
	}
	writeJSONError(w, http.StatusMethodNotAllowed, "method not allowed")
}

func parseHistoryFilter(r *http.Request) (history.SessionFilter, error) {
	query := r.URL.Query()
	filter := history.SessionFilter{
		ServerID: strings.TrimSpace(query.Get("server_id")),
		Owner:    strings.TrimSpace(query.Get("owner")),
		Status:   strings.TrimSpace(query.Get("status")),
		Limit:    parseBoundedInt(query.Get("limit"), 50, 1, 100),
	}
	if query.Get("cursor") != "" {
		cursor, ok := decodeHistoryCursor(query.Get("cursor"))
		if !ok {
			return filter, errors.New("invalid cursor")
		}
		filter.BeforeMS = cursor.At
		filter.BeforeID = cursor.ID
	}
	if filter.Status != "" && filter.Status != "scheduled" && filter.Status != "active" && filter.Status != "completed" && filter.Status != "revoked" {
		return filter, errors.New("status must be scheduled, active, completed, or revoked")
	}
	for value, target := range map[string]**time.Time{"from": &filter.From, "to": &filter.To} {
		text := strings.TrimSpace(query.Get(value))
		if text == "" {
			continue
		}
		parsed, err := time.Parse(time.RFC3339, text)
		if err != nil {
			return filter, errors.New(value + " must be RFC3339")
		}
		parsed = parsed.UTC()
		*target = &parsed
	}
	return filter, nil
}

func parseBoundedInt(value string, fallback, minimum, maximum int) int {
	parsed, err := strconv.Atoi(strings.TrimSpace(value))
	if err != nil || parsed < minimum || parsed > maximum {
		return fallback
	}
	return parsed
}

type historyPageCursor struct {
	At int64  `json:"at"`
	ID string `json:"id"`
}

type historySearchPageCursor struct {
	Field     string   `json:"field"`
	Direction string   `json:"direction"`
	ID        string   `json:"id"`
	Text      *string  `json:"text,omitempty"`
	Number    *float64 `json:"number,omitempty"`
}

func encodeHistorySearchCursor(cursor history.SearchCursor) string {
	data, _ := json.Marshal(historySearchPageCursor{
		Field: cursor.Field, Direction: cursor.Direction, ID: cursor.ID, Text: cursor.Text, Number: cursor.Number,
	})
	return base64.RawURLEncoding.EncodeToString(data)
}

func decodeHistorySearchCursor(value string) (history.SearchCursor, bool) {
	if value == "" {
		return history.SearchCursor{}, true
	}
	if len(value) > 1024 {
		return history.SearchCursor{}, false
	}
	decoded, err := base64.RawURLEncoding.DecodeString(value)
	if err != nil {
		return history.SearchCursor{}, false
	}
	var cursor historySearchPageCursor
	if err := json.Unmarshal(decoded, &cursor); err != nil || cursor.Field == "" || cursor.Direction == "" || cursor.ID == "" || len(cursor.ID) > 256 {
		return history.SearchCursor{}, false
	}
	if (cursor.Text == nil) == (cursor.Number == nil) {
		return history.SearchCursor{}, false
	}
	return history.SearchCursor{Field: cursor.Field, Direction: cursor.Direction, ID: cursor.ID, Text: cursor.Text, Number: cursor.Number}, true
}

func encodeHistoryCursor(at int64, id string) string {
	data, _ := json.Marshal(historyPageCursor{At: at, ID: id})
	return base64.RawURLEncoding.EncodeToString(data)

}

func decodeHistoryCursor(value string) (historyPageCursor, bool) {
	if value == "" {
		return historyPageCursor{}, true
	}
	if len(value) > 512 {
		return historyPageCursor{}, false
	}
	decoded, err := base64.RawURLEncoding.DecodeString(value)
	if err != nil {
		return historyPageCursor{}, false
	}
	var cursor historyPageCursor
	if err := json.Unmarshal(decoded, &cursor); err != nil || cursor.At <= 0 || cursor.ID == "" || len(cursor.ID) > 256 {
		return historyPageCursor{}, false
	}
	return cursor, true
}
