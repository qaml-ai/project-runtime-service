package app

import (
	"encoding/json"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/qaml-ai/project-runtime-service/internal/state"
)

// Usage API routes (control port only):
//
//   GET  /v1/usage/orgs/{orgId}/spend   — lifetime totals
//   GET  /v1/usage/orgs/{orgId}/limits  — effective spend limits
//   PUT  /v1/usage/orgs/{orgId}/limits  — set per-org limit overrides (or clear)
//   GET  /v1/usage/orgs/{orgId}/log         — recent usage log entries (paginated)
//   GET  /v1/usage/orgs/{orgId}/log/sum     — aggregated spend between dates
//   POST /v1/usage/analytics/daily-spend/query — cross-org daily spend aggregation

var usageOrgRouteRegex = regexp.MustCompile(`^/v1/usage/orgs/([^/]+)(/[^/]*(?:/[^/]*)?)$`)

func (s *Server) handleUsageRoute(w http.ResponseWriter, req *http.Request) {
	if strings.HasPrefix(req.URL.Path, "/v1/usage/analytics/") {
		s.handleUsageAnalyticsRoute(w, req)
		return
	}

	match := usageOrgRouteRegex.FindStringSubmatch(req.URL.Path)
	if match == nil {
		errorJSON(w, "Not found", http.StatusNotFound)
		return
	}

	orgID := match[1]
	action := strings.TrimPrefix(match[2], "/")

	switch {
	case action == "spend" && req.Method == http.MethodGet:
		s.handleGetOrgSpend(w, orgID)
	case action == "limits" && req.Method == http.MethodGet:
		s.handleGetOrgLimits(w, orgID)
	case action == "limits" && req.Method == http.MethodPut:
		s.handleSetOrgLimits(w, req, orgID)
	case action == "log" && req.Method == http.MethodGet:
		s.handleGetOrgUsageLog(w, req, orgID)
	case action == "log/sum" && req.Method == http.MethodGet:
		s.handleGetOrgUsageLogSum(w, req, orgID)
	default:
		errorJSON(w, "Not found", http.StatusNotFound)
	}
}

func (s *Server) handleUsageAnalyticsRoute(w http.ResponseWriter, req *http.Request) {
	switch {
	case req.URL.Path == "/v1/usage/analytics/spam-org-ids" && req.Method == http.MethodGet:
		s.handleGetSpamOrgIDs(w)
	case req.URL.Path == "/v1/usage/analytics/orgs/query" && req.Method == http.MethodPost:
		s.handlePostUsageAnalyticsOrgsQuery(w, req)
	case req.URL.Path == "/v1/usage/analytics/daily-spend/query" && req.Method == http.MethodPost:
		s.handlePostUsageAnalyticsDailySpendQuery(w, req)
	default:
		errorJSON(w, "Not found", http.StatusNotFound)
	}
}

func (s *Server) handleGetSpamOrgIDs(w http.ResponseWriter) {
	orgIDs, err := s.usage.ListSpamOrgIDs()
	if err != nil {
		errorJSON(w, "Failed to read spam org ids", http.StatusInternalServerError)
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"org_ids": orgIDs,
		"count":   len(orgIDs),
	})
}

type usageAnalyticsOrgsQueryRequest struct {
	OrgIDs         []string `json:"org_ids"`
	IncludeWindows bool     `json:"include_windows"`
}

func (s *Server) handlePostUsageAnalyticsOrgsQuery(w http.ResponseWriter, req *http.Request) {
	var body usageAnalyticsOrgsQueryRequest
	if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
		errorJSON(w, "Invalid JSON body", http.StatusBadRequest)
		return
	}

	if len(body.OrgIDs) == 0 {
		writeJSON(w, http.StatusOK, map[string]any{
			"items": []state.OrgUsageAnalyticsRow{},
			"count": 0,
		})
		return
	}

	seen := make(map[string]struct{}, len(body.OrgIDs))
	orgIDs := make([]string, 0, len(body.OrgIDs))
	for _, rawOrgID := range body.OrgIDs {
		orgID := strings.TrimSpace(rawOrgID)
		if orgID == "" {
			continue
		}
		if _, ok := seen[orgID]; ok {
			continue
		}
		seen[orgID] = struct{}{}
		orgIDs = append(orgIDs, orgID)
	}

	items, err := s.usage.GetOrgUsageAnalytics(orgIDs, body.IncludeWindows)
	if err != nil {
		errorJSON(w, "Failed to read usage analytics", http.StatusInternalServerError)
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"items": items,
		"count": len(items),
	})
}

type usageAnalyticsDailySpendQueryRequest struct {
	Date         string   `json:"date"`
	OrgIDs       []string `json:"org_ids"`
	TopOrgsLimit int      `json:"top_orgs_limit"`
}

func (s *Server) handlePostUsageAnalyticsDailySpendQuery(w http.ResponseWriter, req *http.Request) {
	var body usageAnalyticsDailySpendQueryRequest
	if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
		errorJSON(w, "Invalid JSON body", http.StatusBadRequest)
		return
	}
	if strings.TrimSpace(body.Date) == "" {
		errorJSON(w, "Date is required", http.StatusBadRequest)
		return
	}
	if _, err := time.Parse("2006-01-02", body.Date); err != nil {
		errorJSON(w, "Invalid date. Expected YYYY-MM-DD.", http.StatusBadRequest)
		return
	}

	response, err := s.usage.GetDailySpendAnalytics(state.DailySpendAnalyticsQuery{
		Date:         body.Date,
		OrgIDs:       body.OrgIDs,
		TopOrgsLimit: body.TopOrgsLimit,
	})
	if err != nil {
		errorJSON(w, "Failed to compute daily spend analytics", http.StatusInternalServerError)
		return
	}

	writeJSON(w, http.StatusOK, response)
}

func (s *Server) handleGetOrgSpend(w http.ResponseWriter, orgID string) {
	spend, err := s.usage.GetOrgSpend(orgID)
	if err != nil {
		errorJSON(w, "Failed to read org spend", http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"org_id":         orgID,
		"total_cost_usd": spend.TotalCostUSD,
		"total_requests": spend.TotalRequests,
		"windows":        []state.WindowSpend{},
	})
}

func (s *Server) handleGetOrgLimits(w http.ResponseWriter, orgID string) {
	limits, err := s.usage.GetSpendLimits(orgID)
	if err != nil {
		errorJSON(w, "Failed to read org limits", http.StatusInternalServerError)
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"org_id": orgID,
		"limits": limits,
	})
}

type setLimitsRequest struct {
	Limits []struct {
		WindowHours float64 `json:"window_hours"`
		LimitUSD    float64 `json:"limit_usd"`
		Label       string  `json:"label"`
	} `json:"limits"`
}

func (s *Server) handleSetOrgLimits(w http.ResponseWriter, req *http.Request, orgID string) {
	var body setLimitsRequest
	if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
		errorJSON(w, "Invalid JSON body", http.StatusBadRequest)
		return
	}

	// Empty limits array = revert to defaults.
	var limits []state.SpendLimit
	for _, l := range body.Limits {
		if l.WindowHours <= 0 || l.LimitUSD <= 0 {
			errorJSON(w, "Each limit must have window_hours > 0 and limit_usd > 0", http.StatusBadRequest)
			return
		}
		label := l.Label
		if label == "" {
			label = formatWindowLabel(l.WindowHours)
		}
		limits = append(limits, state.SpendLimit{
			Window:   time.Duration(l.WindowHours * float64(time.Hour)),
			LimitUSD: l.LimitUSD,
			Label:    label,
		})
	}

	if err := s.usage.SetSpendLimits(orgID, limits); err != nil {
		errorJSON(w, "Failed to set org limits", http.StatusInternalServerError)
		return
	}

	// Return the effective limits (may be defaults if cleared).
	effective, _ := s.usage.GetSpendLimits(orgID)
	writeJSON(w, http.StatusOK, map[string]any{
		"org_id": orgID,
		"limits": effective,
	})
}

func (s *Server) handleGetOrgUsageLog(w http.ResponseWriter, req *http.Request, orgID string) {
	q := state.UsageLogQuery{Limit: 50}

	query := req.URL.Query()
	if l := query.Get("limit"); l != "" {
		if parsed, err := strconv.Atoi(l); err == nil && parsed > 0 && parsed <= 1000 {
			q.Limit = parsed
		}
	}
	if c := query.Get("cursor"); c != "" {
		if parsed, err := strconv.ParseInt(c, 10, 64); err == nil && parsed > 0 {
			q.Cursor = parsed
		}
	}
	if f := query.Get("from"); f != "" {
		if parsed, err := strconv.ParseInt(f, 10, 64); err == nil && parsed > 0 {
			q.FromMs = parsed
		}
	}
	if t := query.Get("to"); t != "" {
		if parsed, err := strconv.ParseInt(t, 10, 64); err == nil && parsed > 0 {
			q.ToMs = parsed
		}
	}

	page, err := s.usage.GetUsageLogPaginated(orgID, q)
	if err != nil {
		errorJSON(w, "Failed to read usage log", http.StatusInternalServerError)
		return
	}

	resp := map[string]any{
		"org_id":      orgID,
		"entries":     page.Entries,
		"count":       page.Count,
		"has_more":    page.HasMore,
		"next_cursor": page.NextCursor,
	}
	if page.NextCursor == "" {
		resp["next_cursor"] = nil
	}
	writeJSON(w, http.StatusOK, resp)
}

func (s *Server) handleGetOrgUsageLogSum(w http.ResponseWriter, req *http.Request, orgID string) {
	query := req.URL.Query()
	fromStr := query.Get("from")
	toStr := query.Get("to")

	if fromStr == "" || toStr == "" {
		errorJSON(w, "Both 'from' and 'to' query params are required (ms timestamps)", http.StatusBadRequest)
		return
	}

	fromMs, err := strconv.ParseInt(fromStr, 10, 64)
	if err != nil || fromMs < 0 {
		errorJSON(w, "Invalid 'from' timestamp", http.StatusBadRequest)
		return
	}
	toMs, err := strconv.ParseInt(toStr, 10, 64)
	if err != nil || toMs < 0 {
		errorJSON(w, "Invalid 'to' timestamp", http.StatusBadRequest)
		return
	}

	chargeableOnly := query.Get("chargeable_only") == "1"
	var sum state.UsageLogSum
	if chargeableOnly {
		sum, err = s.usage.GetCreditChargeableUsageLogSum(orgID, fromMs, toMs)
	} else {
		sum, err = s.usage.GetUsageLogSum(orgID, fromMs, toMs)
	}
	if err != nil {
		errorJSON(w, "Failed to compute usage sum", http.StatusInternalServerError)
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"org_id":                            orgID,
		"total_cost_usd":                    sum.TotalCostUSD,
		"total_requests":                    sum.TotalRequests,
		"total_input_tokens":                sum.TotalInputTokens,
		"total_output_tokens":               sum.TotalOutputTokens,
		"total_cache_creation_input_tokens": sum.TotalCacheCreationInputTokens,
		"total_cache_read_input_tokens":     sum.TotalCacheReadInputTokens,
		"from_ms":                           fromMs,
		"to_ms":                             toMs,
		"chargeable_only":                   chargeableOnly,
	})
}

func formatWindowLabel(hours float64) string {
	if hours >= 24 && int(hours)%24 == 0 {
		return strconv.Itoa(int(hours/24)) + "d"
	}
	if hours == float64(int(hours)) {
		return strconv.Itoa(int(hours)) + "h"
	}
	return strconv.FormatFloat(hours, 'f', 1, 64) + "h"
}
