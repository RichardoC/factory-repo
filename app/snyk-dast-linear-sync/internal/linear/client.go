package linear

import (
	"context"
	"fmt"
	"iter"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	gqlclient "git.sr.ht/~emersion/gqlclient"
	linearapi "github.com/guillermo/linear/linear-api"

	"github.com/tesslio/snyk-dast-linear-sync/internal/config"
	"github.com/tesslio/snyk-dast-linear-sync/internal/httpx"
	"github.com/tesslio/snyk-dast-linear-sync/internal/model"
)

const (
	titlePrefix    = "Snyk DAST:"
	metadataHeader = "<!-- snyk-dast-linear-sync"

	// maxLiveLabelPage is how many labels the live label read requests per issue.
	// It is Linear's per-connection maximum, chosen so the page cannot truncate
	// in practice; if it does anyway, fetchLabelsByIssueID fails that issue
	// closed rather than returning a label set a protected label may be missing
	// from.
	maxLiveLabelPage = 250
)

type Client struct {
	cfg config.LinearConfig
	gql *gqlclient.Client
	log *slog.Logger

	mu              sync.RWMutex
	resolvedTeam    string
	statesByName    map[string]string
	statesByType    map[string]string
	managedLabelIDs map[string]string
	blockedUntil    time.Time
}

// linearIssueNode is the shared GraphQL shape for an issue returned by
// Linear queries. It is kept unexported so both bulk and single-issue lookups
// can share the same conversion logic.
type linearIssueNode struct {
	ID          string  `json:"id"`
	Identifier  string  `json:"identifier"`
	Title       string  `json:"title"`
	Description *string `json:"description"`
	URL         string  `json:"url"`
	Priority    int     `json:"priority"`
	DueDate     *string `json:"dueDate"`
	ArchivedAt  *string `json:"archivedAt"`
	State       struct {
		ID   string `json:"id"`
		Name string `json:"name"`
	} `json:"state"`
	Labels struct {
		Nodes []struct {
			ID   string `json:"id"`
			Name string `json:"name"`
		} `json:"nodes"`
	} `json:"labels"`
}

func New(cfg config.LinearConfig, maxConcurrency int, logger *slog.Logger) *Client {
	base := httpx.NewAdaptiveTransport("linear", maxConcurrency, logger, nil)
	httpClient := &http.Client{
		Transport: &httpx.HeaderTransport{
			Base:  base,
			Key:   "Authorization",
			Value: cfg.APIKey,
		},
	}

	return &Client{
		cfg:             cfg,
		gql:             gqlclient.New("https://api.linear.app/graphql", httpClient),
		log:             logger,
		statesByName:    map[string]string{},
		statesByType:    map[string]string{},
		managedLabelIDs: map[string]string{},
	}
}

func (c *Client) LoadSnapshot(ctx context.Context) ([]model.ExistingIssue, error) {
	if err := c.resolveTeam(ctx); err != nil {
		return nil, err
	}
	if err := c.loadStates(ctx); err != nil {
		return nil, err
	}
	return c.loadIssues(ctx)
}

// LoadIssueByIdentifier fetches a single Linear issue by its identifier (e.g.
// "PROB-12127"). Linear's IssueFilter does not expose an identifier field, so
// the identifier is split into its team key and number and queried by those.
// It does not require states to be loaded, so it is suitable for diagnostic
// tools that only need to inspect one issue.
func (c *Client) LoadIssueByIdentifier(ctx context.Context, identifier string) (model.ExistingIssue, error) {
	teamKey, number, err := parseLinearIdentifier(identifier)
	if err != nil {
		return model.ExistingIssue{}, err
	}

	filter := linearapi.IssueFilter{
		Team: &linearapi.TeamFilter{
			Key: &linearapi.StringComparator{EqIgnoreCase: &teamKey},
		},
		Number: &linearapi.NumberComparator{Eq: &number},
	}

	op := gqlclient.NewOperation(`
query issueByIdentifier($filter: IssueFilter!) {
  issues(filter: $filter, first: 1, includeArchived: true) {
    nodes {
      id
      identifier
      title
      description
      url
      priority
      dueDate
      archivedAt
      state {
        id
        name
      }
      labels {
        nodes {
          id
          name
        }
      }
    }
  }
}`)
	op.Var("filter", filter)

	var resp struct {
		Issues struct {
			Nodes []linearIssueNode `json:"nodes"`
		} `json:"issues"`
	}
	if err := c.execute(ctx, op, &resp); err != nil {
		return model.ExistingIssue{}, fmt.Errorf("fetch Linear issue %q: %w", identifier, err)
	}
	if len(resp.Issues.Nodes) == 0 {
		return model.ExistingIssue{}, fmt.Errorf("Linear issue %q not found", identifier)
	}

	return linearIssueToModel(resp.Issues.Nodes[0]), nil
}

// parseLinearIdentifier splits a Linear identifier such as "PROB-12127" into
// its team key ("PROB") and issue number (12127).
func parseLinearIdentifier(identifier string) (string, float64, error) {
	identifier = strings.TrimSpace(identifier)
	if identifier == "" {
		return "", 0, fmt.Errorf("empty Linear identifier")
	}
	// Linear identifiers are TEAMKEY-NUMBER. The number is always the part
	// after the final dash so identifiers like "FOO-BAR-123" are still handled
	// correctly (team key "FOO-BAR", number 123), although this is unlikely.
	idx := strings.LastIndex(identifier, "-")
	if idx <= 0 || idx == len(identifier)-1 {
		return "", 0, fmt.Errorf("identifier %q is not a valid Linear identifier (expected TEAMKEY-NUMBER)", identifier)
	}
	teamKey := strings.TrimSpace(identifier[:idx])
	numberStr := strings.TrimSpace(identifier[idx+1:])
	if teamKey == "" {
		return "", 0, fmt.Errorf("identifier %q is missing a team key", identifier)
	}
	number, err := strconv.ParseFloat(numberStr, 64)
	if err != nil {
		return "", 0, fmt.Errorf("identifier %q has an invalid issue number: %w", identifier, err)
	}
	return teamKey, number, nil
}

func linearIssueToModel(issue linearIssueNode) model.ExistingIssue {
	description := deref(issue.Description)
	labels := make([]model.IssueLabel, 0, len(issue.Labels.Nodes))
	for _, label := range issue.Labels.Nodes {
		labels = append(labels, model.IssueLabel{
			ID:   label.ID,
			Name: label.Name,
		})
	}
	var archivedAt *time.Time
	if issue.ArchivedAt != nil && strings.TrimSpace(*issue.ArchivedAt) != "" {
		if t, err := time.Parse(time.RFC3339, strings.TrimSpace(*issue.ArchivedAt)); err == nil {
			archivedAt = &t
		}
	}
	return model.ExistingIssue{
		ID:            issue.ID,
		Identifier:    issue.Identifier,
		Title:         issue.Title,
		URL:           issue.URL,
		StateID:       issue.State.ID,
		StateName:     issue.State.Name,
		Description:   description,
		Priority:      issue.Priority,
		DueDate:       deref(issue.DueDate),
		Fingerprint:   extractFingerprint(description),
		ManagedLabels: extractManagedLabels(description),
		Labels:        labels,
		ArchivedAt:    archivedAt,
	}
}

// ExistingFingerprints returns the subset of fingerprints that currently have a
// LIVE (non-archived) managed issue in Linear. It exists to reconcile ambiguous
// write outcomes: when a batch create fails at the transport level, the mutation
// may still have been committed before the response was lost, so retrying blindly
// duplicates tickets. Asking Linear what actually exists resolves that.
//
// Archived issues are deliberately excluded. A live issue is evidence the create
// landed; an archived one is not, and counting it would suppress the deliberate
// replacement ticket created when an archived issue's finding becomes active
// again.
func (c *Client) ExistingFingerprints(ctx context.Context, fingerprints []string) (map[string]struct{}, error) {
	wanted := make(map[string]struct{}, len(fingerprints))
	for _, fingerprint := range fingerprints {
		if trimmed := strings.TrimSpace(fingerprint); trimmed != "" {
			wanted[trimmed] = struct{}{}
		}
	}
	if len(wanted) == 0 {
		return map[string]struct{}{}, nil
	}
	if err := c.resolveTeam(ctx); err != nil {
		return nil, err
	}

	clauses := make([]linearapi.IssueFilter, 0, len(wanted))
	for fingerprint := range wanted {
		clauses = append(clauses, linearapi.IssueFilter{
			Description: &linearapi.NullableStringComparator{
				Contains: new(fingerprint),
			},
		})
	}
	filter := linearapi.IssueFilter{
		Team: &linearapi.TeamFilter{
			Id: &linearapi.IDComparator{Eq: c.teamID()},
		},
		Or: clauses,
	}

	found := map[string]struct{}{}
	var after *string
	for page := 1; ; page++ {
		op := gqlclient.NewOperation(`
query issuesByFingerprint($filter: IssueFilter!, $after: String) {
  issues(first: 100, after: $after, filter: $filter, includeArchived: false) {
    nodes {
      id
      identifier
      description
    }
    pageInfo {
      hasNextPage
      endCursor
    }
  }
}`)
		op.Var("filter", filter)
		op.Var("after", after)

		var resp struct {
			Issues struct {
				Nodes []struct {
					ID          string  `json:"id"`
					Identifier  string  `json:"identifier"`
					Description *string `json:"description"`
				} `json:"nodes"`
				PageInfo struct {
					HasNextPage bool    `json:"hasNextPage"`
					EndCursor   *string `json:"endCursor"`
				} `json:"pageInfo"`
			} `json:"issues"`
		}
		if err := c.execute(ctx, op, &resp); err != nil {
			return nil, fmt.Errorf("look up Linear issues by fingerprint: %w", err)
		}

		for _, issue := range resp.Issues.Nodes {
			fingerprint := extractFingerprint(deref(issue.Description))
			if fingerprint == "" {
				continue
			}
			if _, ok := wanted[fingerprint]; ok {
				found[fingerprint] = struct{}{}
			}
		}

		next := resp.Issues.PageInfo.EndCursor
		if !resp.Issues.PageInfo.HasNextPage || next == nil {
			break
		}
		// A cursor that does not advance would loop forever. The run context may
		// carry no deadline, so this is guarded explicitly rather than relying on
		// cancellation to break out.
		if after != nil && *next == *after {
			return nil, fmt.Errorf("look up Linear issues by fingerprint: pagination cursor did not advance after page %d", page)
		}
		after = next
	}

	return found, nil
}

func (c *Client) StateID(state model.IssueState) (string, error) {
	var name string
	switch state {
	case model.StateTodo:
		name = c.cfg.States.Todo
	case model.StateBacklog:
		name = c.cfg.States.Backlog
	case model.StateDone:
		name = c.cfg.States.Done
	case model.StateCancelled:
		name = c.cfg.States.Cancelled
	default:
		return "", fmt.Errorf("unknown issue state %q", state)
	}

	c.mu.RLock()
	defer c.mu.RUnlock()

	id := c.statesByName[strings.ToLower(name)]
	if id == "" {
		typeID := c.statesByType[stateType(state)]
		if typeID != "" {
			return typeID, nil
		}
		return "", fmt.Errorf("Linear state %q not found for team %s", name, c.teamRef())
	}
	return id, nil
}

// CreateIssues creates a batch of Linear issues in one GraphQL mutation. It
// returns the indices of the entries that failed, so the caller can retry only
// those: a batch mutation can fail per-alias while the rest succeed, and
// retrying the whole batch would create duplicate tickets for the entries that
// were already created. A non-nil error means no per-alias outcome is known
// (e.g. a transport failure), in which case the returned indices are nil.
func (c *Client) CreateIssues(ctx context.Context, desired []model.DesiredIssue) ([]int, error) {
	if len(desired) == 0 {
		return nil, nil
	}
	if err := c.resolveTeam(ctx); err != nil {
		return nil, err
	}
	if err := c.ensureStatesLoaded(ctx); err != nil {
		return nil, err
	}
	if err := c.ensureManagedLabelsResolved(ctx, managedLabelsFromDesiredIssues(desired)); err != nil {
		return nil, err
	}

	op := gqlclient.NewOperation(createIssuesMutation(len(desired)))
	for i, issue := range desired {
		stateID, err := c.StateID(issue.State)
		if err != nil {
			return nil, err
		}

		title := issue.Title
		description := issue.Description
		priority := int32(issue.Priority)
		input := issueCreateInput{
			Title:         &title,
			Description:   &description,
			TeamId:        c.teamID(),
			StateId:       &stateID,
			Priority:      &priority,
			SubscriberIds: c.actorSubscriberIDsForCreate(),
			LabelIds:      c.createLabelIDs(issue),
			DueDate:       stringOrNil(issue.DueDate),
		}
		op.Var(fmt.Sprintf("input%d", i), input)
	}

	resp := map[string]struct {
		Success bool `json:"success"`
		Issue   struct {
			ID         string `json:"id"`
			Identifier string `json:"identifier"`
		} `json:"issue"`
	}{}
	// gqlclient decodes the response body into `resp` BEFORE returning any
	// GraphQL error, so a mutation that failed for individual aliases still
	// leaves the successful ones decoded here. Those issues exist in Linear
	// already: discarding them and reporting a batch error would send the caller
	// into a retry that recreates every one of them. So derive per-alias
	// outcomes whenever anything came back, and only surface the error when
	// nothing did (transport failure, HTTP error, undecodable body).
	if execErr := c.execute(ctx, op, &resp); execErr != nil {
		if len(resp) == 0 {
			return nil, fmt.Errorf("create Linear issues: %w", execErr)
		}
		c.log.Warn("Linear issue create batch reported errors for some entries; the rest were created",
			slog.Int("aliases_returned", len(resp)),
			slog.Int("batch_size", len(desired)),
			slog.Any("error", execErr),
		)
	}

	var failed []int
	for i := range desired {
		alias := fmt.Sprintf("issueCreate%d", i)
		if result, ok := resp[alias]; !ok || !result.Success {
			failed = append(failed, i)
		}
	}
	if len(failed) > 0 {
		c.log.Warn("some Linear issue creates failed without a GraphQL error",
			slog.Int("failed_count", len(failed)),
			slog.Int("batch_size", len(desired)),
		)
	}
	// The issues themselves are already created at this point, so a failure to
	// unsubscribe the actor must not be reported as a create failure — doing so
	// would send the caller into a retry that duplicates every issue in the
	// batch.
	if err := c.issueUnsubscribeCreatedIssues(ctx, resp); err != nil {
		c.log.Warn("failed to unsubscribe actor from newly created Linear issues; the issues themselves were created",
			slog.Any("error", err),
		)
	}

	return failed, nil
}

func (c *Client) UpdateIssues(ctx context.Context, updates []model.IssueUpdate) error {
	if len(updates) == 0 {
		return nil
	}
	if err := c.resolveTeam(ctx); err != nil {
		return err
	}
	if err := c.ensureStatesLoaded(ctx); err != nil {
		return err
	}
	if err := c.ensureManagedLabelsResolved(ctx, managedLabelsFromUpdates(updates)); err != nil {
		return err
	}

	// Re-read the live label set for this batch when any label is protected.
	// An update replaces the ticket's whole label set, so this read is what
	// keeps a protected label another actor added since the run's opening
	// snapshot from being deleted by that replacement.
	var liveLabels map[string][]model.IssueLabel
	if len(c.cfg.Labels.Protected) > 0 {
		issueIDs := make([]string, 0, len(updates))
		for _, update := range updates {
			issueIDs = append(issueIDs, update.Existing.ID)
		}
		fetched, err := c.fetchLabelsByIssueID(ctx, issueIDs)
		if err != nil {
			return err
		}
		liveLabels = fetched

		// An issue missing from the read is indistinguishable from one that
		// genuinely carries no protected label, and guessing wrong deletes
		// another actor's control label. Fail the batch instead: the caller
		// already falls back to updating each issue individually, so the rest of
		// the batch still lands and only the unreadable issue is reported failed.
		for _, update := range updates {
			if _, ok := liveLabels[update.Existing.ID]; !ok {
				return fmt.Errorf("refusing to update Linear issue %s: its current labels could not be read, so protected labels cannot be preserved", update.Existing.Identifier)
			}
		}
	}

	op := gqlclient.NewOperation(updateIssuesMutation(len(updates)))
	for i, update := range updates {
		stateID, err := c.StateID(update.Desired.State)
		if err != nil {
			return err
		}

		title := update.Desired.Title
		description := update.Desired.Description
		priority := int32(update.Desired.Priority)
		labelIDs, err := c.desiredLabelIDs(update.Existing, update.Desired, liveLabels[update.Existing.ID])
		if err != nil {
			return err
		}
		var stateIDPtr *string
		if !update.Desired.PreserveState {
			stateIDPtr = &stateID
		}
		input := issueUpdateInput{
			Title:       &title,
			Description: &description,
			StateId:     stateIDPtr,
			Priority:    &priority,
			LabelIds:    labelIDs,
			DueDate:     stringOrNil(update.Desired.DueDate),
		}
		op.Var(fmt.Sprintf("id%d", i), update.Existing.ID)
		op.Var(fmt.Sprintf("input%d", i), input)
	}

	resp := map[string]struct {
		Success bool `json:"success"`
	}{}
	if err := c.execute(ctx, op, &resp); err != nil {
		return fmt.Errorf("update Linear issues: %w", err)
	}

	for alias, result := range resp {
		if !result.Success {
			return fmt.Errorf("update Linear issues failed without GraphQL error for %s", alias)
		}
	}

	return nil
}

// PostComments posts a single change-summary comment on each updated issue,
// explaining which managed fields were modified and why. Comments are posted
// after a successful update/resolve/cancel mutation so the Linear history
// shows exactly what the sync changed.
// PostComments posts change comments for a batch of updates in one GraphQL
// mutation. Like CreateIssues it returns the indices (into updates) whose
// comment failed, so a partial failure does not make the caller re-post the
// comments that already landed.
func (c *Client) PostComments(ctx context.Context, updates []model.IssueUpdate) ([]int, error) {
	if len(updates) == 0 {
		return nil, nil
	}

	type commentJob struct {
		issueID     string
		comment     string
		updateIndex int
	}
	var jobs []commentJob
	for i, update := range updates {
		if comment := buildChangeComment(update); comment != "" {
			jobs = append(jobs, commentJob{issueID: update.Existing.ID, comment: comment, updateIndex: i})
		}
	}
	if len(jobs) == 0 {
		return nil, nil
	}

	op := gqlclient.NewOperation(commentCreateMutation(len(jobs)))
	for j, job := range jobs {
		input := commentCreateInput{
			Body:          &job.comment,
			IssueId:       job.issueID,
			SubscriberIds: c.actorSubscriberIDsForComment(),
		}
		op.Var(fmt.Sprintf("input%d", j), input)
	}

	resp := map[string]struct {
		Success bool `json:"success"`
		Comment struct {
			ID string `json:"id"`
		} `json:"comment"`
	}{}
	// Same partial-decode reasoning as CreateIssues: comments that posted are
	// already visible on the issue, so only the aliases that actually failed may
	// be retried.
	if execErr := c.execute(ctx, op, &resp); execErr != nil {
		if len(resp) == 0 {
			return nil, fmt.Errorf("post Linear change comments: %w", execErr)
		}
		c.log.Warn("Linear comment batch reported errors for some entries; the rest were posted",
			slog.Int("aliases_returned", len(resp)),
			slog.Int("batch_size", len(jobs)),
			slog.Any("error", execErr),
		)
	}

	var failed []int
	for j, job := range jobs {
		alias := fmt.Sprintf("commentCreate%d", j)
		if result, ok := resp[alias]; !ok || !result.Success {
			failed = append(failed, job.updateIndex)
		}
	}
	if len(failed) > 0 {
		c.log.Warn("some Linear change comments failed without a GraphQL error",
			slog.Int("failed_count", len(failed)),
			slog.Int("batch_size", len(jobs)),
		)
	}

	return failed, nil
}

func (c *Client) loadStates(ctx context.Context) error {
	var after *string
	states := map[string]string{}
	stateTypes := map[string]string{}

	for {
		op := gqlclient.NewOperation(`
query teamStates($id: String!, $after: String) {
  team(id: $id) {
    states(first: 100, after: $after) {
      nodes {
        id
        name
        type
      }
      pageInfo {
        hasNextPage
        endCursor
      }
    }
  }
}`)
		op.Var("id", c.teamID())
		op.Var("after", after)

		var resp struct {
			Team struct {
				States struct {
					Nodes []struct {
						ID   string `json:"id"`
						Name string `json:"name"`
						Type string `json:"type"`
					} `json:"nodes"`
					PageInfo struct {
						HasNextPage bool    `json:"hasNextPage"`
						EndCursor   *string `json:"endCursor"`
					} `json:"pageInfo"`
				} `json:"states"`
			} `json:"team"`
		}
		if err := c.execute(ctx, op, &resp); err != nil {
			return fmt.Errorf("fetch Linear states: %w", err)
		}

		for _, state := range resp.Team.States.Nodes {
			states[strings.ToLower(state.Name)] = state.ID
			if _, exists := stateTypes[state.Type]; !exists {
				stateTypes[state.Type] = state.ID
			}
		}

		if !resp.Team.States.PageInfo.HasNextPage || resp.Team.States.PageInfo.EndCursor == nil {
			break
		}
		after = resp.Team.States.PageInfo.EndCursor
	}

	c.mu.Lock()
	c.statesByName = states
	c.statesByType = stateTypes
	c.mu.Unlock()
	return nil
}

func (c *Client) loadIssues(ctx context.Context) ([]model.ExistingIssue, error) {
	// Linear auto-archives closed issues after the team's configured inactivity
	// period, and archived issues are excluded from the default issues query.
	// Snyk DAST keeps fixed/accepted/invalid findings in its API indefinitely,
	// so without includeArchived the sync would stop seeing its own closed
	// tickets and mint a fresh duplicate for every one of them, every run.
	//
	// Archived issues are bounded to those auto-archived within
	// LINEAR_ARCHIVE_LOOKBACK_DAYS so the snapshot stays a manageable size.
	lookbackDays := c.cfg.ArchiveLookbackDays
	if lookbackDays <= 0 {
		lookbackDays = config.DefaultArchiveLookbackDays // defensive; config.Validate() enforces > 0
	}
	archiveCutoffTime := time.Now().UTC().Add(-time.Duration(lookbackDays) * 24 * time.Hour)
	archiveCutoff := linearapi.DateTime(archiveCutoffTime.Format(time.RFC3339))
	c.log.Info("Linear issue snapshot archive lookback window",
		slog.Int("lookback_days", lookbackDays),
		slog.Time("archive_cutoff", archiveCutoffTime),
	)
	// Per Linear's NullableDateComparator, Null: true matches records where the
	// field IS NULL, i.e. issues that have NOT been auto-archived.
	archivedAtIsNull := true

	// Every managed issue's title is built with the titlePrefix, and the managed
	// label is stamped at create and re-asserted on every update, so either alone
	// identifies a managed ticket. Both are kept as alternatives so that losing
	// one -- a retitled issue, or a label removed by hand -- does not hide the
	// ticket and mint a duplicate.
	identityArms := []linearapi.IssueFilter{
		{Title: &linearapi.StringComparator{StartsWith: new(titlePrefix)}},
	}
	// Only when label management is enabled: with LINEAR_MANAGED_LABEL=off there
	// is no label to match, and an arm matching nothing would narrow the snapshot
	// rather than widen it.
	if managedLabel := strings.TrimSpace(c.cfg.Labels.Managed); managedLabel != "" {
		identityArms = append(identityArms, linearapi.IssueFilter{
			Labels: &linearapi.IssueLabelCollectionFilter{
				Some: &linearapi.IssueLabelFilter{
					Name: &linearapi.StringComparator{EqIgnoreCase: &managedLabel},
				},
			},
		})
	}
	c.log.Info("Linear issue snapshot identity scope",
		slog.String("title_prefix", titlePrefix),
		slog.String("managed_label", c.cfg.Labels.Managed),
		slog.Int("identity_arms", len(identityArms)),
	)
	filter := linearapi.IssueFilter{
		Team: &linearapi.TeamFilter{
			Id: &linearapi.IDComparator{Eq: c.teamID()},
		},
		And: []linearapi.IssueFilter{
			// Archive window. The null arm also matches issues archived manually
			// or via the API (including trashed ones): those have archivedAt set
			// but autoArchivedAt null. They are detected by reading archivedAt
			// back off the issue, not by this filter.
			{
				Or: []linearapi.IssueFilter{
					{AutoArchivedAt: &linearapi.NullableDateComparator{Null: &archivedAtIsNull}},
					{AutoArchivedAt: &linearapi.NullableDateComparator{Gte: &archiveCutoff}},
				},
			},
			// Identity. Both disjuncts are exact predicates -- a title prefix and
			// a label name -- so the snapshot is bounded to this sync's own
			// tickets even when the team is shared with another sync.
			//
			// The metadata block that carries the fingerprint is deliberately NOT
			// used as a query predicate. Filtering on `description: { contains }`
			// did not bound the result set: a run against a team holding another
			// sync's tickets loaded 59,866 issues for 7 findings, i.e. effectively
			// the whole team. Linear does not document `description` as
			// supporting the string comparators at all, so the arm appears to be
			// ignored rather than applied, silently widening the OR to everything.
			// The metadata block is still the authority for identity once an issue
			// is loaded; it just cannot be trusted to select which ones to load.
			{
				Or: identityArms,
			},
		},
	}

	var after *string
	var issues []model.ExistingIssue

	for {
		op := gqlclient.NewOperation(`
query existingIssues($filter: IssueFilter!, $after: String) {
  issues(first: 100, after: $after, filter: $filter, includeArchived: true) {
    nodes {
      id
      identifier
      title
      description
      url
      priority
      dueDate
      archivedAt
      state {
        id
        name
      }
      labels {
        nodes {
          id
          name
        }
      }
    }
    pageInfo {
      hasNextPage
      endCursor
    }
  }
}`)
		op.Var("filter", filter)
		op.Var("after", after)

		var resp struct {
			Issues struct {
				Nodes    []linearIssueNode `json:"nodes"`
				PageInfo struct {
					HasNextPage bool    `json:"hasNextPage"`
					EndCursor   *string `json:"endCursor"`
				} `json:"pageInfo"`
			} `json:"issues"`
		}
		if err := c.execute(ctx, op, &resp); err != nil {
			return nil, fmt.Errorf("fetch Linear issues: %w", err)
		}

		for _, issue := range resp.Issues.Nodes {
			issues = append(issues, linearIssueToModel(issue))
		}

		if !resp.Issues.PageInfo.HasNextPage || resp.Issues.PageInfo.EndCursor == nil {
			break
		}
		after = resp.Issues.PageInfo.EndCursor
	}

	return issues, nil
}

func (c *Client) resolveTeam(ctx context.Context) error {
	c.mu.RLock()
	if c.resolvedTeam != "" {
		c.mu.RUnlock()
		return nil
	}
	c.mu.RUnlock()

	teamRef := c.cfg.TeamID
	if isLikelyUUID(teamRef) {
		c.mu.Lock()
		c.resolvedTeam = teamRef
		c.mu.Unlock()
		return nil
	}

	op := gqlclient.NewOperation(`
query resolveTeam($key: String!) {
  teams(first: 1, filter: { key: { eqIgnoreCase: $key } }) {
    nodes {
      id
      key
      name
    }
  }
}`)
	op.Var("key", teamRef)

	var resp struct {
		Teams struct {
			Nodes []struct {
				ID   string `json:"id"`
				Key  string `json:"key"`
				Name string `json:"name"`
			} `json:"nodes"`
		} `json:"teams"`
	}
	if err := c.execute(ctx, op, &resp); err != nil {
		return fmt.Errorf("resolve Linear team %q: %w", teamRef, err)
	}
	if len(resp.Teams.Nodes) == 0 {
		return fmt.Errorf("Linear team %q was not found by key", teamRef)
	}

	c.mu.Lock()
	c.resolvedTeam = resp.Teams.Nodes[0].ID
	c.mu.Unlock()
	return nil
}

func (c *Client) issueUnsubscribeCreatedIssues(ctx context.Context, created map[string]struct {
	Success bool `json:"success"`
	Issue   struct {
		ID         string `json:"id"`
		Identifier string `json:"identifier"`
	} `json:"issue"`
}) error {
	if !c.cfg.UnsubscribeActor || len(created) == 0 {
		return nil
	}

	issueIDs := make([]string, 0, len(created))
	for _, result := range created {
		if id := strings.TrimSpace(result.Issue.ID); id != "" {
			issueIDs = append(issueIDs, id)
		}
	}
	if len(issueIDs) == 0 {
		return nil
	}

	op := gqlclient.NewOperation(issueUnsubscribeMutation(len(issueIDs)))
	for i, issueID := range issueIDs {
		op.Var(fmt.Sprintf("id%d", i), issueID)
	}

	resp := map[string]struct {
		Success bool `json:"success"`
	}{}
	if err := c.execute(ctx, op, &resp); err != nil {
		return fmt.Errorf("issue-unsubscribe Linear actor from created issues: %w", err)
	}
	for alias, result := range resp {
		if !result.Success {
			return fmt.Errorf("issue-unsubscribe Linear actor from created issues failed without GraphQL error for %s", alias)
		}
	}
	return nil
}

func (c *Client) actorSubscriberIDsForCreate() *[]string {
	if !c.cfg.UnsubscribeActor {
		return nil
	}
	ids := []string{}
	return &ids
}

func (c *Client) actorSubscriberIDsForComment() *[]string {
	if !c.cfg.UnsubscribeActor {
		return nil
	}
	ids := []string{}
	return &ids
}

type issueCreateInput struct {
	Title         *string   `json:"title,omitempty"`
	Description   *string   `json:"description,omitempty"`
	Priority      *int32    `json:"priority,omitempty"`
	SubscriberIds *[]string `json:"subscriberIds,omitempty"`
	LabelIds      []string  `json:"labelIds,omitempty"`
	TeamId        string    `json:"teamId"`
	StateId       *string   `json:"stateId,omitempty"`
	DueDate       *string   `json:"dueDate,omitempty"`
}

type issueUpdateInput struct {
	Title       *string  `json:"title,omitempty"`
	Description *string  `json:"description,omitempty"`
	Priority    *int32   `json:"priority,omitempty"`
	LabelIds    []string `json:"labelIds"`
	StateId     *string  `json:"stateId,omitempty"`
	DueDate     *string  `json:"dueDate"`
}

type commentCreateInput struct {
	Body          *string   `json:"body,omitempty"`
	IssueId       string    `json:"issueId"`
	SubscriberIds *[]string `json:"subscriberIds,omitempty"`
}

func (c *Client) teamID() string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if c.resolvedTeam != "" {
		return c.resolvedTeam
	}
	return c.cfg.TeamID
}

func (c *Client) teamRef() string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if c.resolvedTeam != "" {
		return c.resolvedTeam
	}
	return c.cfg.TeamID
}

func extractFingerprint(description string) string {
	for line := range metadataBlockLines(description) {
		trimmed := strings.TrimSpace(line)
		if after, ok := strings.CutPrefix(trimmed, "fingerprint:"); ok {
			return strings.TrimSpace(after)
		}
		if after, ok := strings.CutPrefix(trimmed, "Fingerprint:"); ok {
			return strings.TrimSpace(after)
		}
	}
	return ""
}

func extractManagedLabels(description string) []string {
	for line := range metadataBlockLines(description) {
		trimmed := strings.TrimSpace(line)
		if after, ok := strings.CutPrefix(trimmed, "managed_labels:"); ok {
			return splitManagedLabels(after)
		}
		if after, ok := strings.CutPrefix(trimmed, "managed_label:"); ok {
			return splitManagedLabels(after)
		}
	}
	return nil
}

// metadataBlockLines yields the lines of the snyk-dast-linear-sync metadata
// block, i.e. the lines between the line-anchored "<!-- snyk-dast-linear-sync"
// marker and its closing "-->". It mirrors the line-boundary anchoring used by
// upsertManagedMetadata so that a marker string appearing mid-sentence in
// user-written text cannot spoof fingerprint/label extraction. If no
// line-anchored block is present, no lines are yielded.
func metadataBlockLines(description string) iter.Seq[string] {
	return func(yield func(string) bool) {
		start := findMetadataBlockStart(description)
		if start < 0 {
			return
		}
		rest := description[start:]
		before, _, ok := strings.Cut(rest, "-->")
		var block string
		if ok {
			block = before
		} else {
			// Unclosed comment (e.g. truncated description): scan to end so the
			// fingerprint is still recoverable.
			block = rest
		}
		for line := range strings.SplitSeq(block, "\n") {
			if !yield(line) {
				return
			}
		}
	}
}

// findMetadataBlockStart locates the snyk-dast-linear-sync metadata block start
// marker in the description, anchored to the beginning of a line. This
// prevents false matches where the marker string appears mid-sentence in
// user-written text (e.g. "See <!-- snyk-dast-linear-sync notes -->"), which
// could spoof fingerprint/label extraction if treated as a metadata block.
func findMetadataBlockStart(description string) int {
	header := metadataHeader
	for i := 0; i <= len(description)-len(header); {
		idx := strings.Index(description[i:], header)
		if idx < 0 {
			return -1
		}
		absIdx := i + idx
		if absIdx == 0 || description[absIdx-1] == '\n' {
			return absIdx
		}
		i = absIdx + 1
	}
	return -1
}

func (c *Client) ensureStatesLoaded(ctx context.Context) error {
	c.mu.RLock()
	loaded := len(c.statesByName) > 0 || len(c.statesByType) > 0
	c.mu.RUnlock()
	if loaded {
		return nil
	}
	return c.loadStates(ctx)
}

func (c *Client) ensureManagedLabelsResolved(ctx context.Context, labels []string) error {
	for _, label := range normalizeManagedLabelNames(labels) {
		if err := c.resolveManagedLabel(ctx, label); err != nil {
			return err
		}
	}
	return nil
}

func (c *Client) resolveManagedLabel(ctx context.Context, managedLabel string) error {
	managedLabel = strings.TrimSpace(managedLabel)
	if managedLabel == "" {
		return nil
	}

	c.mu.RLock()
	if c.managedLabelIDs[model.NormalizeLabelName(managedLabel)] != "" {
		c.mu.RUnlock()
		return nil
	}
	c.mu.RUnlock()

	var after *string
	var teamMatches []string
	var globalMatches []string
	for {
		op := gqlclient.NewOperation(`
query managedIssueLabels($name: String!, $after: String) {
  issueLabels(first: 100, after: $after, filter: { name: { eqIgnoreCase: $name } }) {
    nodes {
      id
      name
      team {
        id
      }
    }
    pageInfo {
      hasNextPage
      endCursor
    }
  }
}`)
		op.Var("name", managedLabel)
		op.Var("after", after)

		var resp struct {
			IssueLabels struct {
				Nodes []struct {
					ID   string `json:"id"`
					Name string `json:"name"`
					Team *struct {
						ID string `json:"id"`
					} `json:"team"`
				} `json:"nodes"`
				PageInfo struct {
					HasNextPage bool    `json:"hasNextPage"`
					EndCursor   *string `json:"endCursor"`
				} `json:"pageInfo"`
			} `json:"issueLabels"`
		}
		if err := c.execute(ctx, op, &resp); err != nil {
			return fmt.Errorf("fetch Linear labels: %w", err)
		}

		for _, label := range resp.IssueLabels.Nodes {
			if !strings.EqualFold(label.Name, managedLabel) {
				continue
			}
			switch {
			case label.Team != nil && label.Team.ID == c.teamID():
				teamMatches = append(teamMatches, label.ID)
			case label.Team == nil:
				globalMatches = append(globalMatches, label.ID)
			}
		}

		if !resp.IssueLabels.PageInfo.HasNextPage || resp.IssueLabels.PageInfo.EndCursor == nil {
			break
		}
		after = resp.IssueLabels.PageInfo.EndCursor
	}

	var resolved string
	switch {
	case len(teamMatches) == 1:
		resolved = teamMatches[0]
	case len(teamMatches) > 1:
		return fmt.Errorf("managed Linear label %q is ambiguous for team %s; keep only one matching label", managedLabel, c.teamRef())
	case len(globalMatches) == 1:
		resolved = globalMatches[0]
	case len(globalMatches) > 1:
		return fmt.Errorf("managed Linear label %q is ambiguous across workspace labels; keep only one matching label", managedLabel)
	default:
		return fmt.Errorf("managed Linear label %q was not found; create the label in Linear or set LINEAR_MANAGED_LABEL=off", managedLabel)
	}

	c.mu.Lock()
	c.managedLabelIDs[model.NormalizeLabelName(managedLabel)] = resolved
	c.mu.Unlock()
	return nil
}

// createLabelIDs resolves the labels stamped on a brand-new issue. A create has
// no pre-existing label set to replace, so protected labels need no special
// handling here: there is nothing of another actor's to clobber.
func (c *Client) createLabelIDs(desired model.DesiredIssue) []string {
	c.mu.RLock()
	defer c.mu.RUnlock()

	out := make([]string, 0, len(desired.ManagedLabels))
	for _, label := range normalizeManagedLabelNames(desired.ManagedLabels) {
		id := c.managedLabelIDs[label]
		if id == "" {
			continue
		}
		out = append(out, id)
	}
	return out
}

// desiredLabelIDs computes the full label set to write for an update, since
// Linear's issueUpdate replaces the ticket's labels wholesale rather than
// applying a delta.
//
// live carries the ticket's label set as read immediately before the write (nil
// when no labels are protected). Protected labels are taken from live and from
// nowhere else: the `existing` snapshot was taken at the top of the run, so a
// protected label another actor added since then is absent from it, and echoing
// that stale set back to Linear would delete the label. Everything else still
// comes from the snapshot, which is the pre-existing behaviour.
func (c *Client) desiredLabelIDs(existing model.ExistingIssue, desired model.DesiredIssue, live []model.IssueLabel) ([]string, error) {
	desiredManaged := normalizeManagedLabelNames(desired.ManagedLabels)
	protected := c.protectedLabelNames()
	out := make([]string, 0, len(existing.Labels)+len(desiredManaged))
	seen := make(map[string]struct{}, len(existing.Labels)+len(desiredManaged))
	previousManaged := make(map[string]struct{}, len(existing.ManagedLabels))
	for _, label := range normalizeManagedLabelNames(existing.ManagedLabels) {
		previousManaged[label] = struct{}{}
	}

	// Protected labels come from the live read below, so skip them here: the
	// snapshot's view of them is stale in both directions (it can miss one that
	// was just added, and carry one that was just removed).
	for _, label := range existing.Labels {
		normalized := model.NormalizeLabelName(label.Name)
		if _, isProtected := protected[normalized]; isProtected {
			continue
		}
		if _, managed := previousManaged[normalized]; managed {
			continue
		}
		if _, exists := seen[label.ID]; exists {
			continue
		}
		out = append(out, label.ID)
		seen[label.ID] = struct{}{}
	}

	// Carry over every protected label the ticket actually has right now. This
	// preserves a label another actor added mid-run and equally respects one it
	// just removed (an absent label is simply never re-added).
	for _, label := range live {
		normalized := model.NormalizeLabelName(label.Name)
		if _, isProtected := protected[normalized]; !isProtected {
			continue
		}
		if _, exists := seen[label.ID]; exists {
			continue
		}
		out = append(out, label.ID)
		seen[label.ID] = struct{}{}
	}

	if len(desiredManaged) == 0 {
		return out, nil
	}

	c.mu.RLock()
	defer c.mu.RUnlock()
	for _, managedLabel := range desiredManaged {
		// A protected label must never be asserted from the managed set — that
		// would let a misconfiguration stamp another actor's control label onto
		// a ticket that does not have one.
		if _, isProtected := protected[managedLabel]; isProtected {
			continue
		}
		managedLabelID := c.managedLabelIDs[managedLabel]
		if managedLabelID == "" {
			return nil, fmt.Errorf("managed Linear label %q was not resolved", managedLabel)
		}
		if _, exists := seen[managedLabelID]; exists {
			continue
		}
		out = append(out, managedLabelID)
		seen[managedLabelID] = struct{}{}
	}
	return out, nil
}

// protectedLabelNames returns the configured protected labels as a normalized
// set for membership tests.
func (c *Client) protectedLabelNames() map[string]struct{} {
	if len(c.cfg.Labels.Protected) == 0 {
		return nil
	}
	out := make(map[string]struct{}, len(c.cfg.Labels.Protected))
	for _, label := range c.cfg.Labels.Protected {
		if normalized := model.NormalizeLabelName(label); normalized != "" {
			out[normalized] = struct{}{}
		}
	}
	return out
}

// fetchLabelsByIssueID reads the current label set of specific issues, keyed by
// issue ID. It is the authority for protected labels immediately before an
// update, so it deliberately re-queries Linear rather than reusing the run's
// snapshot.
//
// includeArchived is set because an archived ticket can still be the target of
// an update. The issue side needs no pagination: the update batch size (10)
// bounds it, and `first` is set from the batch.
//
// The label side is a connection and therefore can truncate, so its pageInfo is
// checked. A truncated label page is not usable as the authority for protected
// labels — a protected label past the page boundary would be absent from the
// live read, and the update would then delete it, which is the exact failure
// this read exists to prevent. Such an issue is therefore omitted from the
// result, which UpdateIssues treats as unreadable and refuses. Refusing beats
// paginating here: a nested connection cannot be paged in this one query, and an
// issue with more than maxLiveLabelPage labels is pathological rather than a
// case worth serving. The refusal is loud (a warning plus a failed op, which
// fails the run) instead of silently dropping a label.
//
// The complexity cost is roughly maxLiveLabelPage x batch size per update batch,
// well inside Linear's 10,000-per-query limit.
func (c *Client) fetchLabelsByIssueID(ctx context.Context, issueIDs []string) (map[string][]model.IssueLabel, error) {
	if len(issueIDs) == 0 {
		return nil, nil
	}

	op := gqlclient.NewOperation(fmt.Sprintf(`
query issueLabelsByID($filter: IssueFilter!, $first: Int!) {
  issues(filter: $filter, first: $first, includeArchived: true) {
    nodes {
      id
      labels(first: %d) {
        nodes {
          id
          name
        }
        pageInfo {
          hasNextPage
        }
      }
    }
  }
}`, maxLiveLabelPage))
	op.Var("filter", linearapi.IssueFilter{
		Id: &linearapi.IDComparator{In: issueIDs},
	})
	op.Var("first", len(issueIDs))

	var resp struct {
		Issues struct {
			Nodes []struct {
				ID     string `json:"id"`
				Labels struct {
					Nodes []struct {
						ID   string `json:"id"`
						Name string `json:"name"`
					} `json:"nodes"`
					PageInfo struct {
						HasNextPage bool `json:"hasNextPage"`
					} `json:"pageInfo"`
				} `json:"labels"`
			} `json:"nodes"`
		} `json:"issues"`
	}
	if err := c.execute(ctx, op, &resp); err != nil {
		return nil, fmt.Errorf("fetch live Linear labels: %w", err)
	}

	out := make(map[string][]model.IssueLabel, len(resp.Issues.Nodes))
	for _, node := range resp.Issues.Nodes {
		if node.Labels.PageInfo.HasNextPage {
			c.log.Warn("Linear issue carries more labels than one page of the live label read; refusing to update it rather than risk deleting a protected label",
				slog.String("issue_id", node.ID),
				slog.Int("page_size", maxLiveLabelPage),
			)
			continue
		}
		labels := make([]model.IssueLabel, 0, len(node.Labels.Nodes))
		for _, label := range node.Labels.Nodes {
			labels = append(labels, model.IssueLabel{ID: label.ID, Name: label.Name})
		}
		out[node.ID] = labels
	}
	return out, nil
}

func splitManagedLabels(raw string) []string {
	parts := strings.Split(raw, ",")
	out := make([]string, 0, len(parts))
	for _, part := range parts {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		out = append(out, part)
	}
	return normalizeManagedLabelNames(out)
}

func managedLabelsFromDesiredIssues(desired []model.DesiredIssue) []string {
	out := make([]string, 0, len(desired))
	for _, issue := range desired {
		out = append(out, issue.ManagedLabels...)
	}
	return normalizeManagedLabelNames(out)
}

func managedLabelsFromUpdates(updates []model.IssueUpdate) []string {
	out := make([]string, 0, len(updates))
	for _, update := range updates {
		out = append(out, update.Desired.ManagedLabels...)
	}
	return normalizeManagedLabelNames(out)
}

func deref(value *string) string {
	if value == nil {
		return ""
	}
	return *value
}

//go:fix inline
func stringPtr(value string) *string {
	return new(value)
}

func stringOrNil(value string) *string {
	if strings.TrimSpace(value) == "" {
		return nil
	}
	return &value
}

func isLikelyUUID(value string) bool {
	if len(value) != 36 {
		return false
	}
	dashes := 0
	for i, r := range value {
		switch {
		case r == '-':
			if i != 8 && i != 13 && i != 18 && i != 23 {
				return false
			}
			dashes++
		case r >= '0' && r <= '9':
		case r >= 'a' && r <= 'f':
		case r >= 'A' && r <= 'F':
		default:
			return false
		}
	}
	return dashes == 4
}

func stateType(state model.IssueState) string {
	switch state {
	case model.StateTodo:
		return "unstarted"
	case model.StateBacklog:
		return "backlog"
	case model.StateDone:
		return "completed"
	case model.StateCancelled:
		return "canceled"
	default:
		return ""
	}
}

func normalizeManagedLabelNames(values []string) []string {
	return model.NormalizeManagedLabelNames(values)
}

// buildChangeComment generates a human-readable comment explaining why the
// sync changed a managed issue. Unlike the Linear activity log which shows
// what changed, this comment explains why — linking each change back to the
// Snyk DAST finding state that drove it. It returns an empty string when there is
// nothing meaningful to report.
func buildChangeComment(update model.IssueUpdate) string {
	if update.Diff == nil {
		return ""
	}
	d := update.Diff
	if !d.TitleChanged && !d.DescriptionChanged && !d.DueDateChanged &&
		!d.StateChanged && !d.PriorityChanged && len(d.LabelsAdded) == 0 && len(d.LabelsRemoved) == 0 {
		return ""
	}

	lines := []string{"**snyk-dast-linear-sync**", ""}

	if d.StateChanged {
		reason := update.Desired.StateReason
		if reason != "" {
			lines = append(lines, fmt.Sprintf("- Moved to **%s** — %s", d.StateTo, reason))
		} else {
			lines = append(lines, fmt.Sprintf("- Moved to **%s**", d.StateTo))
		}
	}

	if d.DueDateChanged {
		if update.Desired.DueDate == "" {
			reason := update.Desired.DueDateReason
			if reason != "" {
				lines = append(lines, fmt.Sprintf("- Due date cleared — %s", reason))
			} else {
				lines = append(lines, "- Due date cleared")
			}
		} else {
			reason := update.Desired.DueDateReason
			if reason != "" {
				lines = append(lines, fmt.Sprintf("- Due date set to **%s** — %s", update.Desired.DueDate, reason))
			} else {
				lines = append(lines, fmt.Sprintf("- Due date set to **%s**", update.Desired.DueDate))
			}
		}
	}

	if d.DescriptionChanged {
		lines = append(lines, "- Description updated — Snyk DAST finding data changed")
	}

	if d.TitleChanged {
		lines = append(lines, "- Title updated — Snyk DAST finding data changed")
	}

	if d.PriorityChanged {
		lines = append(lines, fmt.Sprintf("- Priority set to **%s** — Snyk DAST severity changed", priorityName(d.PriorityTo)))
	}

	for _, label := range d.LabelsAdded {
		var reason string
		if update.Desired.LabelReasons != nil {
			reason = update.Desired.LabelReasons[label]
		}
		if reason != "" {
			lines = append(lines, fmt.Sprintf("- Added **%s** — %s", label, reason))
		} else {
			lines = append(lines, fmt.Sprintf("- Added **%s**", label))
		}
	}

	for _, label := range d.LabelsRemoved {
		lines = append(lines, fmt.Sprintf("- Removed **%s** — no longer applicable", label))
	}

	return strings.Join(lines, "\n")
}

func priorityName(priority int) string {
	switch priority {
	case 1:
		return "Urgent"
	case 2:
		return "High"
	case 3:
		return "Medium"
	case 4:
		return "Low"
	default:
		return "None"
	}
}

func createIssuesMutation(size int) string {
	var builder strings.Builder
	builder.WriteString("mutation issueCreateBatch(")
	for i := range size {
		if i > 0 {
			builder.WriteString(", ")
		}
		builder.WriteString(fmt.Sprintf("$input%d: IssueCreateInput!", i))
	}
	builder.WriteString(") {\n")
	for i := range size {
		builder.WriteString(fmt.Sprintf("  issueCreate%d: issueCreate(input: $input%d) {\n", i, i))
		builder.WriteString("    success\n")
		builder.WriteString("    issue {\n")
		builder.WriteString("      id\n")
		builder.WriteString("      identifier\n")
		builder.WriteString("    }\n")
		builder.WriteString("  }\n")
	}
	builder.WriteString("}")
	return builder.String()
}

func updateIssuesMutation(size int) string {
	var builder strings.Builder
	builder.WriteString("mutation issueUpdateBatch(")
	for i := range size {
		if i > 0 {
			builder.WriteString(", ")
		}
		builder.WriteString(fmt.Sprintf("$id%d: String!, $input%d: IssueUpdateInput!", i, i))
	}
	builder.WriteString(") {\n")
	for i := range size {
		builder.WriteString(fmt.Sprintf("  issueUpdate%d: issueUpdate(id: $id%d, input: $input%d) {\n", i, i, i))
		builder.WriteString("    success\n")
		builder.WriteString("  }\n")
	}
	builder.WriteString("}")
	return builder.String()
}

func issueUnsubscribeMutation(size int) string {
	var builder strings.Builder
	builder.WriteString("mutation issueUnsubscribeBatch(")
	for i := range size {
		if i > 0 {
			builder.WriteString(", ")
		}
		builder.WriteString(fmt.Sprintf("$id%d: String!", i))
	}
	builder.WriteString(") {\n")
	for i := range size {
		builder.WriteString(fmt.Sprintf("  issueUnsubscribe%d: issueUnsubscribe(id: $id%d) {\n", i, i))
		builder.WriteString("    success\n")
		builder.WriteString("  }\n")
	}
	builder.WriteString("}")
	return builder.String()
}

func commentCreateMutation(size int) string {
	var builder strings.Builder
	builder.WriteString("mutation commentCreateBatch(")
	for i := range size {
		if i > 0 {
			builder.WriteString(", ")
		}
		builder.WriteString(fmt.Sprintf("$input%d: CommentCreateInput!", i))
	}
	builder.WriteString(") {\n")
	for i := range size {
		builder.WriteString(fmt.Sprintf("  commentCreate%d: commentCreate(input: $input%d) {\n", i, i))
		builder.WriteString("    success\n")
		builder.WriteString("    comment {\n")
		builder.WriteString("      id\n")
		builder.WriteString("    }\n")
		builder.WriteString("  }\n")
	}
	builder.WriteString("}")
	return builder.String()
}

func (c *Client) execute(ctx context.Context, op *gqlclient.Operation, out any) error {
	var lastErr error

	for attempt := range 6 {
		if err := c.waitForRateLimitWindow(ctx); err != nil {
			return err
		}

		if err := c.gql.Execute(ctx, op, out); err != nil {
			lastErr = err
			if !isLinearRateLimitError(err) {
				return err
			}

			delay := linearRateLimitBackoff(attempt)
			c.noteRateLimit(delay)
			c.log.Warn("Linear GraphQL rate limit reached",
				slog.Duration("retry_after", delay),
				slog.Int("attempt", attempt+1),
			)

			timer := time.NewTimer(delay)
			select {
			case <-ctx.Done():
				timer.Stop()
				return ctx.Err()
			case <-timer.C:
			}
			continue
		}

		return nil
	}

	return lastErr
}

func (c *Client) waitForRateLimitWindow(ctx context.Context) error {
	for {
		c.mu.RLock()
		until := c.blockedUntil
		c.mu.RUnlock()

		if until.IsZero() || time.Now().After(until) {
			return nil
		}

		timer := time.NewTimer(time.Until(until))
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
}

func (c *Client) noteRateLimit(delay time.Duration) {
	until := time.Now().Add(delay)

	c.mu.Lock()
	if until.After(c.blockedUntil) {
		c.blockedUntil = until
	}
	c.mu.Unlock()
}

func isLinearRateLimitError(err error) bool {
	if err == nil {
		return false
	}

	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "ratelimit exceeded") || strings.Contains(msg, "rate limit exceeded")
}

func linearRateLimitBackoff(attempt int) time.Duration {
	backoff := 5 * time.Second
	for range attempt {
		backoff *= 2
		if backoff >= time.Minute {
			return time.Minute
		}
	}
	return backoff
}
