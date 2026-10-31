package snykdast

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"log/slog"

	"github.com/tesslio/snyk-dast-linear-sync/internal/model"
	"golang.org/x/sync/errgroup"
)

const (
	pageLength = 100

	severityCritical = "critical"
	severityHigh     = "high"
	severityMedium   = "medium"
	severityLow      = "low"
)

// targetRef holds the subset of a Snyk DAST target (scope) needed to render and
// deduplicate findings.
type targetRef struct {
	ID   string
	Name string
	Type string
	URL  string
	Host string
}

type targetNode struct {
	ID   string `json:"id"`
	Type string `json:"type"`
	Site struct {
		Name string `json:"name"`
		URL  string `json:"url"`
		Host string `json:"host"`
	} `json:"site"`
}

type findingNode struct {
	// ID is the numeric finding id. The OpenAPI spec documents this as a
	// string in <TARGET_ID>-<FINDING_ID> format, but the live API returns a
	// bare integer. json.Number accepts both shapes and lets us stringify it
	// uniformly for fingerprint construction and URL building.
	ID             json.Number `json:"id"`
	State          string      `json:"state"`
	Severity       int         `json:"severity"`
	CVSSScore      *float64    `json:"cvss_score"`
	URL            string      `json:"url"`
	Path           string      `json:"path"`
	Method         string      `json:"method"`
	Parameter      string      `json:"parameter"`
	Insertion      string      `json:"insertion_point"`
	Fix            string      `json:"fix"`
	Evidence       string      `json:"evidence"`
	CreatedAt      *string     `json:"created_at"`
	LastFound      string      `json:"last_found"`
	ExpirationDate *string     `json:"expiration_date"`
	// HasSASTCorrelations is not in the published OpenAPI spec but is present
	// in live API responses. It is the cheapest signal for whether the
	// per-finding correlation endpoint will return any data, so the sync uses
	// it to skip unnecessary correlation calls for uncorrelated findings.
	HasSASTCorrelations bool `json:"has_sast_correlations"`
	Definition          struct {
		ID      string `json:"id"`
		Name    string `json:"name"`
		Desc    string `json:"desc"`
		CWEID   string `json:"cwe_id"`
		CWEName string `json:"cwe_name"`
	} `json:"definition"`
	Target targetSummary `json:"target"`
}

// targetSummary is the embedded target (SimpleScope) inside a finding. It
// carries enough to render context even if the targets list is somehow
// missing an entry.
type targetSummary struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	Type string `json:"type"`
}

// definitionNode is the shape of GET /definitions/. The `passive` boolean
// distinguishes passive checks (response analysis, e.g. missing security
// headers) from active checks (exploit payloads, e.g. SQL injection, XSS).
// This field is not documented in the published OpenAPI spec (see
// the published OpenAPI schema) but is present in live API responses.
type definitionNode struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	Passive *bool  `json:"passive"`
}

func (c *Client) LoadSnapshot(ctx context.Context) (model.SnykDASTSnapshot, error) {
	targets, err := c.listTargets(ctx)
	if err != nil {
		return model.SnykDASTSnapshot{}, err
	}

	targetByID := make(map[string]targetRef, len(targets))
	targetIDs := make(map[string]struct{}, len(targets))
	for _, target := range targets {
		targetByID[target.ID] = target
		targetIDs[target.ID] = struct{}{}
	}

	findings, err := c.listFindings(ctx)
	if err != nil {
		return model.SnykDASTSnapshot{}, err
	}

	definitions, err := c.listDefinitions(ctx)
	if err != nil {
		return model.SnykDASTSnapshot{}, err
	}

	normalized := make([]model.Finding, 0, len(findings))
	correlated := make([]bool, 0, len(findings))
	for _, finding := range findings {
		targetID := strings.TrimSpace(finding.Target.ID)
		if targetID == "" {
			// The finding's embedded target summary has no id. The finding id is a
			// bare numeric id (not composite), so there is no way to recover the
			// target id from it. Skip the finding rather than emit an empty
			// fingerprint that cannot be deduplicated.
			continue
		}

		target, ok := targetByID[targetID]
		var targetName, targetType, targetURL, targetHost string
		if ok {
			targetName = target.Name
			targetType = target.Type
			targetURL = target.URL
			targetHost = target.Host
		} else {
			// Target was deleted between the targets and findings calls, or the
			// finding references a target the API key cannot see. Use the
			// summary embedded in the finding so the issue is still actionable.
			targetName = finding.Target.Name
			targetType = finding.Target.Type
		}

		status, ignoreExpiresAt := mapStatus(finding.State, finding.ExpirationDate)
		findingIDStr := finding.ID.String()
		if raw, unparseable := unparseableAcceptanceExpiry(finding.State, finding.ExpirationDate); unparseable {
			// The acceptance carries an expiry but it cannot be parsed, so
			// mapStatus treats the acceptance as permanent (ignored ->
			// Cancelled). That silently cancels a reopened-ticket candidate,
			// so say so: name the finding and the value that failed to parse.
			c.logger.Warn("accepted finding has an unparseable expiration_date; treating the acceptance as permanent",
				slog.String("finding_id", findingIDStr),
				slog.String("expiration_date", raw))
		}

		normalized = append(normalized, model.Finding{
			Fingerprint:       model.Fingerprint(targetID, findingIDStr),
			SnykDASTFindingID: findingIDStr,
			DefinitionID:      finding.Definition.ID,
			CreatedAt:         parseTimePtr(finding.CreatedAt),
			LastFound:         parseTime(finding.LastFound),
			TargetID:          targetID,
			TargetName:        coalesce(targetName, finding.Target.Name, targetHost),
			TargetType:        targetType,
			TargetURL:         targetURL,
			TargetHost:        targetHost,
			IssueTitle:        coalesce(finding.Definition.Name, finding.Definition.ID, findingIDStr),
			Severity:          mapSeverity(finding.Severity),
			CVSS:              derefFloat(finding.CVSSScore),
			CWE:               finding.Definition.CWEID,
			CWEName:           finding.Definition.CWEName,
			Fix:               finding.Fix,
			Evidence:          finding.Evidence,
			FindingURL:        finding.URL,
			IssueURL:          c.findingUIURL(targetID, findingIDStr),
			IssueAPIURL:       c.findingAPIURL(findingIDStr),
			Status:            status,
			Method:            finding.Method,
			Path:              finding.Path,
			Parameter:         finding.Parameter,
			InsertionPoint:    finding.Insertion,
			CheckType:         checkTypeForDefinition(definitions, finding.Definition.ID),
			IgnoreExpiresAt:   ignoreExpiresAt,
		})
		correlated = append(correlated, finding.HasSASTCorrelations)
	}

	if err := c.enrichCorrelations(ctx, normalized, correlated); err != nil {
		return model.SnykDASTSnapshot{}, err
	}

	return model.SnykDASTSnapshot{
		Findings:          normalized,
		TargetIDs:         targetIDs,
		InactiveTargetIDs: map[string]struct{}{},
	}, nil
}

func (c *Client) ListFindings(ctx context.Context) ([]model.Finding, error) {
	snapshot, err := c.LoadSnapshot(ctx)
	if err != nil {
		return nil, err
	}
	return snapshot.Findings, nil
}

// pageEnvelope is the shared shape of every paginated Snyk DAST listing:
// count is the server total number of records, page_total the total number of
// pages, and results the current page. The node type behind results differs
// per endpoint, so it stays raw until the caller decodes it.
type pageEnvelope struct {
	Count     int             `json:"count"`
	PageTotal int             `json:"page_total"`
	Results   json.RawMessage `json:"results"`
}

// listPages walks one paginated Snyk DAST listing. It GETs path (relative to
// the API base) once per page, adding length and page to query (callers may
// pass other filters such as team) and hands each page's raw results array to
// onPage.
//
// The loop stops when page_total says the listing has ended. Two conditions
// are truncation rather than the end, and return an error:
//
//   - a page that comes back empty before page_total is reached: records the
//     count promised were never served;
//   - fewer results accumulated in total than the count the last page
//     reported. This catches page_total absent or 0 (a schema change, a proxy
//     that strips it, an error page that decodes as an empty struct), which
//     would otherwise stop after page 1 and silently accept a partial listing.
//
// A total above count (records added between pages) is fine. The error
// message names the path and the two numbers, nothing else.
func (c *Client) listPages(ctx context.Context, path string, query url.Values, onPage func(results json.RawMessage) error) error {
	got := 0
	count := 0
	page := 1
	for {
		endpoint, err := c.apiBase.Parse(path)
		if err != nil {
			return fmt.Errorf("build %s URL: %w", path, err)
		}
		q := endpoint.Query()
		for key, values := range query {
			for _, value := range values {
				q.Add(key, value)
			}
		}
		q.Set("length", strconv.Itoa(pageLength))
		q.Set("page", strconv.Itoa(page))
		endpoint.RawQuery = q.Encode()

		req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint.String(), nil)
		if err != nil {
			return err
		}
		req.Header.Set("Accept", "application/json")

		resp, err := c.httpClient.Do(req)
		if err != nil {
			return err
		}
		var payload pageEnvelope
		if err := c.decodeJSON(resp, &payload); err != nil {
			return err
		}

		rows, err := countResults(path, payload.Results)
		if err != nil {
			return err
		}
		if err := onPage(payload.Results); err != nil {
			return err
		}

		got += len(rows)
		count = payload.Count

		if len(rows) == 0 && payload.PageTotal > page {
			return fmt.Errorf("Snyk DAST GET %s returned %d of %d results", path, got, count)
		}
		if payload.PageTotal <= page {
			break
		}
		page++
	}
	if got < count {
		return fmt.Errorf("Snyk DAST GET %s returned %d of %d results", path, got, count)
	}
	return nil
}

// countResults decodes a page's raw results array far enough to count its
// entries. An absent or null results field counts as zero entries.
func countResults(path string, raw json.RawMessage) ([]json.RawMessage, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return nil, nil
	}
	var rows []json.RawMessage
	if err := json.Unmarshal(raw, &rows); err != nil {
		return nil, fmt.Errorf("decode Snyk DAST %s results: %w", path, err)
	}
	return rows, nil
}

// decodeResults unmarshals a page's raw results array into out. An absent or
// null results field leaves out untouched.
func decodeResults(path string, raw json.RawMessage, out any) error {
	if len(raw) == 0 || string(raw) == "null" {
		return nil
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return fmt.Errorf("decode Snyk DAST %s results: %w", path, err)
	}
	return nil
}

func (c *Client) listTargets(ctx context.Context) ([]targetRef, error) {
	targets := make([]targetRef, 0, 128)

	query := url.Values{}
	if team := strings.TrimSpace(c.cfg.Team); team != "" {
		query.Set("team", team)
	}
	err := c.listPages(ctx, "targets/", query, func(results json.RawMessage) error {
		var nodes []targetNode
		if err := decodeResults("targets/", results, &nodes); err != nil {
			return err
		}
		for _, node := range nodes {
			name := strings.TrimSpace(node.Site.Name)
			if name == "" {
				name = node.Site.Host
			}
			targets = append(targets, targetRef{
				ID:   node.ID,
				Name: name,
				Type: node.Type,
				URL:  node.Site.URL,
				Host: node.Site.Host,
			})
		}
		return nil
	})
	if err != nil {
		return nil, err
	}

	c.logger.Info("loaded Snyk DAST targets", slog.Int("count", len(targets)))
	return targets, nil
}

// listDefinitions fetches all vulnerability definitions and returns a map
// from definition ID to check type ("passive" or "active"). The check type
// is the Snyk DAST equivalent of Snyk's multi-product "tool" dimension.
func (c *Client) listDefinitions(ctx context.Context) (map[string]string, error) {
	out := make(map[string]string)
	err := c.listPages(ctx, "definitions/", nil, func(results json.RawMessage) error {
		var nodes []definitionNode
		if err := decodeResults("definitions/", results, &nodes); err != nil {
			return err
		}
		for _, def := range nodes {
			if def.ID == "" {
				continue
			}
			if def.Passive != nil && *def.Passive {
				out[def.ID] = "passive"
			} else if def.Passive != nil {
				out[def.ID] = "active"
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	c.logger.Info("loaded Snyk DAST definitions", slog.Int("count", len(out)))
	return out, nil
}

// checkTypeForDefinition returns "passive" or "active" for a definition ID,
// or "" if the definition is not in the map (e.g. the definition endpoint
// returned no data for it).
func checkTypeForDefinition(definitions map[string]string, defID string) string {
	if ct, ok := definitions[strings.TrimSpace(defID)]; ok {
		return ct
	}
	return ""
}

func (c *Client) listFindings(ctx context.Context) ([]findingNode, error) {
	findings := make([]findingNode, 0, 256)

	// Restrict to targets in the configured team (if any) so a multi-team
	// API key does not pull findings from unrelated targets.
	query := url.Values{}
	if team := strings.TrimSpace(c.cfg.Team); team != "" {
		query.Set("team", team)
	}
	err := c.listPages(ctx, "findings/", query, func(results json.RawMessage) error {
		var nodes []findingNode
		if err := decodeResults("findings/", results, &nodes); err != nil {
			return err
		}
		findings = append(findings, nodes...)
		return nil
	})
	if err != nil {
		return nil, err
	}

	c.logger.Info("loaded Snyk DAST findings", slog.Int("count", len(findings)))
	return findings, nil
}

// mapStatus converts a Snyk DAST finding state into the canonical model status.
//
//   - notfixed / retesting -> open (retesting is a transient re-verification
//     state; the vulnerability is still present until proven otherwise)
//   - fixed -> fixed
//   - invalid -> ignored (false positive / not a real vulnerability)
//   - accepted with a future expiration_date -> snoozed (time-limited risk
//     acceptance); the SLA clock restarts from the acceptance expiry
//   - accepted with a past expiration_date -> open (the acceptance has
//     lapsed and the finding needs attention again)
//   - accepted without an expiration_date, or with one that cannot be
//     parsed -> ignored (permanent risk acceptance). An unparseable expiry
//     is additionally reported with a warning by LoadSnapshot so the
//     permanent cancellation is visible.
//
// correlationNode is one entry from the Snyk Code (SAST) correlation
// endpoint. The `markdown` field is a pre-rendered human-readable block
// describing the linked source vulnerability (repo/file/line).
type correlationNode struct {
	ID        string `json:"id"`
	FactID    string `json:"fact_id"`
	Confirmed bool   `json:"confirmed"`
	Markdown  string `json:"markdown"`
}

type paginatedCorrelations struct {
	Count     int               `json:"count"`
	Page      int               `json:"page"`
	PageTotal int               `json:"page_total"`
	Length    int               `json:"length"`
	Results   []correlationNode `json:"results"`
}

// enrichCorrelations fetches Snyk Code (SAST) correlation markdown for
// findings that have them and attaches the rendered blocks to each Finding.
// It uses the undocumented `has_sast_correlations` boolean on each finding
// (the published schema omits it) to identify the subset that needs a
// correlation fetch, avoiding a separate /findings/?snyk_sast=true call.
// Findings with no Snyk Code correlation incur zero extra HTTP calls.
func (c *Client) enrichCorrelations(ctx context.Context, findings []model.Finding, correlated []bool) error {
	count := 0
	for _, has := range correlated {
		if has {
			count++
		}
	}
	c.logger.Info("loaded Snyk Code correlated findings", slog.Int("count", count))
	if count == 0 {
		return nil
	}

	g, gctx := errgroup.WithContext(ctx)
	// Bound concurrency to the configured Snyk DAST HTTP concurrency. The
	// adaptive transport also limits in-flight requests, but this semaphore
	// avoids spawning a goroutine per finding when thousands have correlations.
	sem := make(chan struct{}, c.concurrency)

	for i := range findings {
		if !correlated[i] {
			continue
		}
		targetID := findings[i].TargetID
		numeric := numericFindingID(findings[i].SnykDASTFindingID)
		if targetID == "" || numeric == "" {
			continue
		}
		pos := i
		compositeID := findings[i].SnykDASTFindingID
		fingerprint := findings[i].Fingerprint
		sem <- struct{}{}
		g.Go(func() error {
			defer func() { <-sem }()
			blocks, err := c.fetchCorrelationMarkdown(gctx, targetID, numeric, fingerprint)
			if err != nil {
				// The missing-permission error describes the whole fetch, not
				// one finding: return it unchanged so the run fails fast with
				// the message naming correlation_viewer.
				if errors.Is(err, ErrCorrelationForbidden) {
					return err
				}
				return fmt.Errorf("fetch snyk code correlations for finding %s: %w", compositeID, err)
			}
			findings[pos].CorrelationMarkdown = blocks
			return nil
		})
	}

	return g.Wait()
}

// ErrCorrelationForbidden reports that the Snyk DAST API key cannot read the
// Snyk Code correlation endpoint. It aborts the run: without the permission
// every correlated finding would lose its description block. The message
// names the missing permission and deliberately omits the target and finding
// URL, so it is safe to paste into an issue.
var ErrCorrelationForbidden = errors.New("Snyk DAST API GET correlations failed with 401/403: the API key is missing the correlation_viewer permission; add it to the custom role (see README, Snyk DAST (Probely) API Permissions)")

// fetchCorrelationMarkdown returns the pre-rendered markdown blocks for the
// Snyk Code correlations of a single finding. The endpoint is
// /targets/{target_id}/findings/{finding_id}/integrations/snyk-sast/correlations/
// where finding_id is the numeric finding id (suffix of the composite id).
//
// Two statuses are handled specially, everything else fails the snapshot:
//
//   - 401/403 maps to ErrCorrelationForbidden (the API key lacks the
//     correlation_viewer permission) so the run fails with a diagnosable
//     message instead of a URL;
//   - 404 for one finding is tolerated as "no correlations" (warn log, no
//     block): a correlation removed between the findings listing and this
//     fetch produces the same description as one that never existed, so
//     degrading it does not churn the description on the next run.
func (c *Client) fetchCorrelationMarkdown(ctx context.Context, targetID, numericFindingID, fingerprint string) ([]string, error) {
	var blocks []string
	path := fmt.Sprintf("targets/%s/findings/%s/integrations/snyk-sast/correlations/", targetID, numericFindingID)
	err := c.listPages(ctx, path, nil, func(results json.RawMessage) error {
		var nodes []correlationNode
		if err := decodeResults(path, results, &nodes); err != nil {
			return err
		}
		for _, node := range nodes {
			if md := strings.TrimSpace(node.Markdown); md != "" {
				blocks = append(blocks, md)
			}
		}
		return nil
	})
	if err != nil {
		if apiErr, ok := errors.AsType[*apiError](err); ok {
			switch apiErr.Status {
			case http.StatusUnauthorized, http.StatusForbidden:
				return nil, ErrCorrelationForbidden
			case http.StatusNotFound:
				c.logger.Warn("Snyk DAST Code correlations disappeared for finding, continuing without the description block",
					slog.String("fingerprint", fingerprint))
				return nil, nil
			}
		}
		return nil, err
	}
	return blocks, nil
}

func mapStatus(state string, expirationDate *string) (model.FindingStatus, time.Time) {
	switch strings.ToLower(strings.TrimSpace(state)) {
	case "fixed":
		return model.FindingFixed, time.Time{}
	case "invalid":
		return model.FindingIgnored, time.Time{}
	case "accepted":
		if expirationDate != nil {
			expires, activeUntil, ok := parseExpirationDate(*expirationDate)
			if ok {
				if time.Now().Before(activeUntil) {
					// Acceptance still in force: track the finding but do not
					// treat it as needing attention yet.
					return model.FindingSnoozed, expires
				}
				// The acceptance was time-limited and has lapsed. A lapsed
				// acceptance is not a permanent one: the finding needs
				// attention again, so it reopens with the SLA measured from the
				// expiry date (see issueDueDate). Mapping it to ignored instead
				// would silently cancel the ticket at the exact moment the team
				// asked to be reminded about it.
				return model.FindingOpen, expires
			}
		}
		// An acceptance with no expiry (or an unparseable one) is permanent.
		return model.FindingIgnored, time.Time{}
	default: // notfixed, retesting, or unknown
		return model.FindingOpen, time.Time{}
	}
}

// unparseableAcceptanceExpiry reports whether a finding is an accepted one
// whose expiration_date is present but cannot be parsed (see
// parseExpirationDate). mapStatus treats such an acceptance as permanent
// (ignored, which the sync maps to Cancelled); the caller uses this helper to
// log a warning naming the finding and the value, so the permanent
// cancellation is visible instead of silent.
func unparseableAcceptanceExpiry(state string, expirationDate *string) (raw string, unparseable bool) {
	// An empty or blank value is no expiry at all, which mapStatus already
	// documents as a permanent acceptance: nothing failed to parse.
	if expirationDate == nil || strings.TrimSpace(*expirationDate) == "" || !strings.EqualFold(strings.TrimSpace(state), "accepted") {
		return "", false
	}
	if _, _, ok := parseExpirationDate(*expirationDate); ok {
		return "", false
	}
	return *expirationDate, true
}

// parseExpirationDate parses a Snyk DAST acceptance expiry. The API documents a
// plain date, but a full RFC3339 timestamp is accepted too so that a format
// change upstream does not silently turn every time-limited acceptance into a
// permanent one.
//
// It returns two instants. expires is the expiry as written, and is what the
// SLA due date is measured from. activeUntil is the instant the acceptance
// actually stops applying, and is what "has it lapsed?" must be compared
// against. They differ for a date-only value: an expiry of 2026-08-19 means the
// acceptance covers all of the 19th, so it lapses at the END of that day.
// Comparing against midnight instead would expire the acceptance a day early,
// pulling the finding back into triage while the team still considers it
// accepted. An RFC3339 value carries its own time, so the two coincide.
func parseExpirationDate(raw string) (expires, activeUntil time.Time, ok bool) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return time.Time{}, time.Time{}, false
	}
	if parsed, err := time.Parse(time.DateOnly, raw); err == nil && !parsed.IsZero() {
		return parsed, parsed.AddDate(0, 0, 1), true
	}
	if parsed, err := time.Parse(time.RFC3339, raw); err == nil && !parsed.IsZero() {
		return parsed, parsed, true
	}
	return time.Time{}, time.Time{}, false
}

func mapSeverity(code int) string {
	switch code {
	case 40:
		return severityCritical
	case 30:
		return severityHigh
	case 20:
		return severityMedium
	case 10:
		return severityLow
	default:
		return "unknown"
	}
}

// findingAPIURL returns the REST URL for a single finding. The /findings/{id}/
// endpoint takes the numeric finding id, which is the suffix of the composite
// global id (<TARGET_ID>-<FINDING_ID>).
func (c *Client) findingAPIURL(compositeID string) string {
	numeric := numericFindingID(compositeID)
	if numeric == "" {
		return ""
	}
	endpoint, err := c.apiBase.Parse(fmt.Sprintf("findings/%s/", numeric))
	if err != nil {
		return ""
	}
	return endpoint.String()
}

// findingUIURL returns a best-effort link into the Snyk DAST web application.
// The exact app routing is not part of the public API contract, so the base
// URL is configurable via SNYK_DAST_APP_BASE.
func (c *Client) findingUIURL(targetID, compositeID string) string {
	targetID = strings.TrimSpace(targetID)
	numeric := numericFindingID(compositeID)
	if targetID == "" || numeric == "" {
		return ""
	}
	base, err := url.Parse(strings.TrimRight(c.cfg.AppBase, "/"))
	if err != nil {
		return ""
	}
	base.Path = fmt.Sprintf("/targets/%s/findings/%s", targetID, numeric)
	return base.String()
}

// numericFindingID extracts the numeric finding id from a composite global id
// of the form <TARGET_ID>-<FINDING_ID>. Snyk DAST target ids are Base58 strings
// (which never contain "-"), so the segment after the final dash is the
// numeric finding id.
func numericFindingID(compositeID string) string {
	compositeID = strings.TrimSpace(compositeID)
	if compositeID == "" {
		return ""
	}
	idx := strings.LastIndex(compositeID, "-")
	if idx < 0 {
		return compositeID
	}
	return strings.TrimSpace(compositeID[idx+1:])
}

func parseTime(raw string) time.Time {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return time.Time{}
	}
	if t, err := time.Parse(time.RFC3339Nano, raw); err == nil {
		return t
	}
	if t, err := time.Parse(time.RFC3339, raw); err == nil {
		return t
	}
	return time.Time{}
}

func parseTimePtr(raw *string) time.Time {
	if raw == nil {
		return time.Time{}
	}
	return parseTime(*raw)
}

func derefFloat(value *float64) float64 {
	if value == nil {
		return 0
	}
	return *value
}

func coalesce(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}
