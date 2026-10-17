package linear

import (
	"context"
	"errors"
	"fmt"
	"iter"
	"log/slog"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	gqlclient "git.sr.ht/~emersion/gqlclient"
	linearapi "github.com/guillermo/linear/linear-api"

	"github.com/tesslio/snyk-linear-sync/internal/config"
	"github.com/tesslio/snyk-linear-sync/internal/httpx"
	"github.com/tesslio/snyk-linear-sync/internal/model"
)

const (
	titlePrefix    = "Snyk:"
	metadataHeader = "<!-- snyk-linear-sync"
)

type Client struct {
	cfg config.LinearConfig
	gql *gqlclient.Client
	log *slog.Logger

	mu           sync.RWMutex
	resolvedTeam string
	statesByName map[string]string
	statesByType map[string]string
	// statesByID maps each of the team's workflow state IDs to its name and
	// type ("triage", "backlog", "unstarted", "started", "completed",
	// "canceled"), for reading state IDs out of issue history.
	statesByID      map[string]workflowState
	managedLabelIDs map[string]string
	// managedLabelGroupIDs maps a normalized managed-label name to the ID of its
	// Linear exclusive group (or "" when ungrouped), so desiredLabelIDs can drop
	// a carried-forward sibling when the sync asserts its own child of that group.
	managedLabelGroupIDs map[string]string
	blockedUntil         time.Time
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
	CreatedAt   *string `json:"createdAt"`
	CompletedAt *string `json:"completedAt"`
	CanceledAt  *string `json:"canceledAt"`
	State       struct {
		ID   string `json:"id"`
		Name string `json:"name"`
	} `json:"state"`
	Labels struct {
		Nodes []struct {
			ID     string `json:"id"`
			Name   string `json:"name"`
			Parent *struct {
				ID string `json:"id"`
			} `json:"parent"`
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
		cfg:                  cfg,
		gql:                  gqlclient.New("https://api.linear.app/graphql", httpClient),
		log:                  logger,
		statesByName:         map[string]string{},
		statesByType:         map[string]string{},
		managedLabelIDs:      map[string]string{},
		managedLabelGroupIDs: map[string]string{},
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
// "SNYK-12127"). Linear's IssueFilter does not expose an identifier field, so
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
      createdAt
      completedAt
      canceledAt
      state {
        id
        name
      }
      labels(first: 250) {
        nodes {
          id
          name
          parent {
            id
          }
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

// parseLinearIdentifier splits a Linear identifier such as "SNYK-12127" into
// its team key ("SNYK") and issue number (12127).
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
		groupID := ""
		if label.Parent != nil {
			groupID = label.Parent.ID
		}
		labels = append(labels, model.IssueLabel{
			ID:      label.ID,
			Name:    label.Name,
			GroupID: groupID,
		})
	}
	// A ticket moved Done -> Cancelled (or back and forth) can carry both
	// timestamps; the later one is when it entered its current terminal
	// state. Linear clears them when a ticket is reopened.
	closedAt := parseLinearTime(issue.CompletedAt)
	if canceledAt := parseLinearTime(issue.CanceledAt); canceledAt != nil && (closedAt == nil || canceledAt.After(*closedAt)) {
		closedAt = canceledAt
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
		ArchivedAt:    parseLinearTime(issue.ArchivedAt),
		CreatedAt:     parseLinearTime(issue.CreatedAt),
		ClosedAt:      closedAt,
		Identity:      extractIdentity(description),
		ClosedReason:  extractClosedReason(description),
	}
}

// parseLinearTime parses an optional Linear DateTime. A missing or
// unparsable value yields nil, which every caller treats as "unknown".
func parseLinearTime(value *string) *time.Time {
	if value == nil || *value == "" {
		return nil
	}
	t, err := time.Parse(time.RFC3339, *value)
	if err != nil {
		return nil
	}
	return &t
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

// CreateIssues creates the given issues in Linear as a single batched
// GraphQL mutation using aliased issueCreate calls.
//
// It returns the indices (into desired) of items whose alias reported
// success: false. A GraphQL alias failing does not mean its sibling aliases
// in the same request failed too — Linear already created those issues, and
// the caller must not recreate them (doing so would produce duplicate
// tickets). The caller should retry only the returned indices individually.
//
// A non-nil error means the request itself could not be completed (e.g. a
// transport/HTTP failure) and no per-alias results are available at all. In
// that case the caller cannot tell which, if any, items succeeded and must
// fall back to retrying every item in desired individually — that is
// unavoidable without idempotency keys on the mutation.
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
	if err := c.execute(ctx, op, &resp); err != nil {
		return nil, fmt.Errorf("create Linear issues: %w", err)
	}

	var failed []int
	for i := range desired {
		alias := fmt.Sprintf("issueCreate%d", i)
		if result, ok := resp[alias]; !ok || !result.Success {
			failed = append(failed, i)
		}
	}

	if err := c.issueUnsubscribeCreatedIssues(ctx, resp); err != nil {
		// The issues above were already created successfully; unsubscribing
		// the actor is best-effort cleanup (it only stops the sync's own
		// actor from being subscribed to notifications on tickets it
		// manages). A failure here must NOT be reported as a create
		// failure — the caller would otherwise recreate issues that already
		// exist, producing duplicates.
		c.log.Warn("failed to unsubscribe actor from newly created Linear issues; issues were created successfully",
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

	// Re-read the live label set for this batch, always. An update replaces the
	// ticket's whole label set, so echoing back the run's opening snapshot
	// deletes any label another actor added since then. The sync owns only its
	// managed labels; every other label — protected, human-applied, or from a
	// different automation — is carried forward from this live read, which is
	// the ticket's current truth rather than the minutes-old snapshot.
	issueIDs := make([]string, 0, len(updates))
	for _, update := range updates {
		issueIDs = append(issueIDs, update.Existing.ID)
	}
	liveLabels, err := c.fetchLabelsByIssueID(ctx, issueIDs)
	if err != nil {
		return err
	}

	// An issue missing from the read leaves its current labels unknown, and
	// guessing wrong deletes a label the sync does not own (a kikimora control
	// label, or a human's). Fail the batch instead: the caller already falls
	// back to updating each issue individually, so the rest of the batch still
	// lands and only the unreadable issue is reported failed.
	for _, update := range updates {
		if _, ok := liveLabels[update.Existing.ID]; !ok {
			return fmt.Errorf("refusing to update Linear issue %s: its current labels could not be read, so labels the sync does not own cannot be preserved", update.Existing.Identifier)
		}
	}

	op := gqlclient.NewOperation(updateIssuesMutation(len(updates)))
	for i, update := range updates {
		stateID, err := c.StateID(update.Desired.State)
		if err != nil {
			return err
		}
		if update.Desired.RestoreState.ID != "" {
			stateID = update.Desired.RestoreState.ID
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
//
// It returns the indices (into updates) whose comment failed to post, so the
// caller can retry only those individually — the same rationale as
// CreateIssues: a sibling alias failing does not mean this comment failed
// too, and retrying a comment that already posted would leave a duplicate
// notification comment on the issue. Updates that had nothing to comment on
// (buildChangeComment returned "") are never included in the failed list.
//
// A non-nil error means the request itself could not be completed (e.g. a
// transport/HTTP failure) with no per-alias results available; the caller
// must then retry every update individually, as before.
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
	if err := c.execute(ctx, op, &resp); err != nil {
		return nil, fmt.Errorf("post Linear change comments: %w", err)
	}

	var failed []int
	for j, job := range jobs {
		alias := fmt.Sprintf("commentCreate%d", j)
		if result, ok := resp[alias]; !ok || !result.Success {
			failed = append(failed, job.updateIndex)
		}
	}

	return failed, nil
}

func (c *Client) loadStates(ctx context.Context) error {
	var after *string
	states := map[string]string{}
	stateTypes := map[string]string{}
	statesByID := map[string]workflowState{}

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
			statesByID[state.ID] = workflowState{name: state.Name, stateType: state.Type}
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
	c.statesByID = statesByID
	c.mu.Unlock()
	return nil
}

// workflowState is one of the team's workflow states, as the client caches
// it for reading state IDs out of issue history.
type workflowState struct {
	name      string
	stateType string
}

// open reports whether the state is one a ticket is worked in, as opposed to
// a closed (completed or canceled) one.
func (s workflowState) open() bool {
	return s.stateType != "completed" && s.stateType != "canceled"
}

// maxHistoryPages bounds how many pages of one issue's history LastOpenStates
// reads. A synced ticket gains history on every update, but nowhere near
// this many entries.
const maxHistoryPages = 20

// stateTransition is one workflow state change in an issue's history.
type stateTransition struct {
	at          time.Time
	fromStateID string
	toStateID   string
}

// LastOpenStates reads the history of each issue and returns, per issue ID,
// the open workflow state the issue was last in before it was closed (see
// lastOpenState). Issues whose history yields none are absent. Issues whose
// history cannot be read are reported in the returned error and left out;
// the rest are still returned.
func (c *Client) LastOpenStates(ctx context.Context, issueIDs []string) (map[string]model.WorkflowState, error) {
	if len(issueIDs) == 0 {
		return nil, nil
	}
	if err := c.resolveTeam(ctx); err != nil {
		return nil, err
	}
	if err := c.ensureStatesLoaded(ctx); err != nil {
		return nil, err
	}

	out := make(map[string]model.WorkflowState, len(issueIDs))
	var errs []error
	for _, issueID := range issueIDs {
		transitions, err := c.fetchStateTransitions(ctx, issueID)
		if err != nil {
			errs = append(errs, fmt.Errorf("read history of Linear issue %s: %w", issueID, err))
			if ctx.Err() != nil {
				break
			}
			continue
		}
		if state, ok := c.lastOpenState(transitions); ok {
			out[issueID] = state
		}
	}
	return out, errors.Join(errs...)
}

// fetchStateTransitions returns every workflow state change in the issue's
// history, in the order Linear returns them.
func (c *Client) fetchStateTransitions(ctx context.Context, issueID string) ([]stateTransition, error) {
	var (
		after       *string
		transitions []stateTransition
	)
	for range maxHistoryPages {
		op := gqlclient.NewOperation(`
query issueStateHistory($id: String!, $after: String) {
  issue(id: $id) {
    history(first: 100, after: $after) {
      nodes {
        createdAt
        fromStateId
        toStateId
      }
      pageInfo {
        hasNextPage
        endCursor
      }
    }
  }
}`)
		op.Var("id", issueID)
		op.Var("after", after)

		var resp struct {
			Issue struct {
				History struct {
					Nodes []struct {
						CreatedAt   string  `json:"createdAt"`
						FromStateID *string `json:"fromStateId"`
						ToStateID   *string `json:"toStateId"`
					} `json:"nodes"`
					PageInfo struct {
						HasNextPage bool    `json:"hasNextPage"`
						EndCursor   *string `json:"endCursor"`
					} `json:"pageInfo"`
				} `json:"history"`
			} `json:"issue"`
		}
		if err := c.execute(ctx, op, &resp); err != nil {
			return nil, fmt.Errorf("fetch Linear issue history: %w", err)
		}

		for _, node := range resp.Issue.History.Nodes {
			toStateID := deref(node.ToStateID)
			if toStateID == "" {
				// Not a state change.
				continue
			}
			at, err := time.Parse(time.RFC3339, node.CreatedAt)
			if err != nil {
				return nil, fmt.Errorf("parse Linear issue history time %q: %w", node.CreatedAt, err)
			}
			transitions = append(transitions, stateTransition{
				at:          at,
				fromStateID: deref(node.FromStateID),
				toStateID:   toStateID,
			})
		}

		page := resp.Issue.History.PageInfo
		if !page.HasNextPage || page.EndCursor == nil {
			return transitions, nil
		}
		after = page.EndCursor
	}
	return nil, fmt.Errorf("issue history is longer than %d pages", maxHistoryPages)
}

// lastOpenState returns the latest open workflow state the transitions show
// the issue in: the target of a transition into an open state, or the origin
// of a transition into a closed one. Linear does not record the state an
// issue was created in, but the first transition out of it does. States that
// are not the team's current states (the issue came from another team, or
// the state was archived since) are skipped, because an update cannot move
// the issue there.
func (c *Client) lastOpenState(transitions []stateTransition) (model.WorkflowState, bool) {
	slices.SortStableFunc(transitions, func(a, b stateTransition) int {
		return a.at.Compare(b.at)
	})

	c.mu.RLock()
	defer c.mu.RUnlock()
	last := ""
	for _, t := range transitions {
		if state, ok := c.statesByID[t.toStateID]; ok && state.open() {
			last = t.toStateID
		} else if state, ok := c.statesByID[t.fromStateID]; ok && state.open() {
			last = t.fromStateID
		}
	}
	if last == "" {
		return model.WorkflowState{}, false
	}
	return model.WorkflowState{ID: last, Name: c.statesByID[last].name}, true
}

func (c *Client) loadIssues(ctx context.Context) ([]model.ExistingIssue, error) {
	// Include archived issues so the sync can still see recently-closed
	// tickets (the reopen guard needs them). Without this, auto-archiving
	// would make closed tickets invisible after the archive period, and the
	// sync would create duplicates if Snyk re-reports the same issue.
	//
	// The OR filter limits archived tickets to those auto-archived within
	// the configured lookback window (LINEAR_ARCHIVE_LOOKBACK_DAYS, default
	// 35 days / 5 weeks). This keeps the snapshot from growing unbounded
	// while still covering the auto-archive period with a margin.
	lookbackDays := c.cfg.ArchiveLookbackDays
	if lookbackDays <= 0 {
		lookbackDays = 35 // defensive; config.Validate() enforces > 0
	}
	archiveCutoffTime := time.Now().UTC().Add(-time.Duration(lookbackDays) * 24 * time.Hour)
	archiveCutoff := archiveCutoffTime.Format(time.RFC3339)
	archiveCutoffDate := linearapi.DateTime(archiveCutoff)
	c.log.Info("Linear loadIssues archive lookback window",
		slog.Int("lookback_days", lookbackDays),
		slog.Time("archive_cutoff", archiveCutoffTime),
	)
	// archivedAtIsNull is passed as the Null comparator on AutoArchivedAt.
	// Per Linear's NullableDateComparator: "Matches any non-null values if
	// the given value is false, otherwise it matches null values." So
	// null: true matches issues where autoArchivedAt IS NULL (i.e. NOT
	// archived) -- which is what the first two OR clauses below intend.
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
	c.log.Info("Linear loadIssues identity scope",
		slog.String("title_prefix", titlePrefix),
		slog.String("managed_label", c.cfg.Labels.Managed),
		slog.Int("identity_arms", len(identityArms)),
	)

	filter := linearapi.IssueFilter{
		Team: &linearapi.TeamFilter{
			Id: &linearapi.IDComparator{Eq: c.teamID()},
		},
		And: []linearapi.IssueFilter{
			// Archive window.
			{
				Or: []linearapi.IssueFilter{
					{AutoArchivedAt: &linearapi.NullableDateComparator{Null: &archivedAtIsNull}},
					{AutoArchivedAt: &linearapi.NullableDateComparator{Gte: &archiveCutoffDate}},
				},
			},
			// Identity. Both disjuncts are exact predicates -- a title prefix and
			// a label name -- so the snapshot is bounded to this sync's own
			// tickets even when the Linear team is shared with another sync.
			//
			// The metadata block that carries the fingerprint is deliberately NOT
			// used as a query predicate. Filtering on `description: { contains }`
			// did not bound the result set: the sibling snyk-dast-linear-sync,
			// which uses this same filter shape against this same team, loaded
			// 59,866 issues for 7 findings -- effectively every issue in the team.
			// Linear does not document `description` as supporting the string
			// comparators at all, so the arm appears to be ignored rather than
			// applied, silently widening the OR to everything. The metadata block
			// is still the authority for identity once an issue is loaded; it just
			// cannot be trusted to select which ones to load.
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
      createdAt
      completedAt
      canceledAt
      state {
        id
        name
      }
      labels(first: 250) {
        nodes {
          id
          name
          parent {
            id
          }
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

// extractFingerprint returns the canonical form of the stored fingerprint:
// Linear's editor rewrites markdown-equivalent sequences inside the stored
// description (e.g. __main__.py becomes **main**.py), so the raw stored
// value cannot be compared against computed fingerprints directly.
func extractFingerprint(description string) string {
	for line := range metadataBlockLines(description) {
		trimmed := strings.TrimSpace(line)
		if after, ok := strings.CutPrefix(trimmed, "fingerprint:"); ok {
			return model.CanonicalFingerprint(strings.TrimSpace(after))
		}
		if after, ok := strings.CutPrefix(trimmed, "Fingerprint:"); ok {
			return model.CanonicalFingerprint(strings.TrimSpace(after))
		}
	}
	return ""
}

// extractIdentity returns the project-independent finding identity from the
// metadata block, or "" when the ticket predates the identity line. The
// value is lowercase hex, which Linear's markdown normalization leaves
// alone, but it is canonicalized the same way as the fingerprint (escapes
// stripped) so a future rewrite cannot silently break rebinding.
func extractIdentity(description string) string {
	return metadataValue(description, "identity:")
}

// extractClosedReason returns the closed_reason the sync recorded in the
// metadata block when it cancelled the ticket because its Snyk project
// disappeared, or "".
func extractClosedReason(description string) string {
	return metadataValue(description, "closed_reason:")
}

// metadataValue returns the canonicalized, lowercased value of the first
// metadata block line starting with prefix, or "". Linear may escape
// punctuation such as '-' and '_' (closed_reason: project\-missing), so the
// value goes through the same canonicalization as the fingerprint.
func metadataValue(description, prefix string) string {
	for line := range metadataBlockLines(description) {
		// Linear can also escape the underscore in the key itself.
		trimmed := model.CanonicalFingerprint(strings.TrimSpace(line))
		if after, ok := strings.CutPrefix(trimmed, prefix); ok {
			return strings.ToLower(strings.TrimSpace(after))
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

// metadataBlockLines yields the lines of the snyk-linear-sync metadata block,
// i.e. the lines between the line-anchored "<!-- snyk-linear-sync" marker and
// its closing "-->". It mirrors the line-boundary anchoring used by
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

// findMetadataBlockStart locates the snyk-linear-sync metadata block start
// marker in the description, anchored to the beginning of a line. This
// prevents false matches where the marker string appears mid-sentence in
// user-written text (e.g. "See <!-- snyk-linear-sync notes -->"), which
// could spoof fingerprint/label extraction if treated as a metadata block.
//
// It returns the LAST line-anchored occurrence, not the first. Ticket
// descriptions can embed free-form Snyk-controlled prose (e.g. issue
// description/remediation text) ABOVE the real metadata block, since the
// sync always appends the managed metadata block last. If that prose
// happens to contain a line-anchored marker (e.g. quoted/copied from
// elsewhere), returning the first occurrence would hijack
// extractFingerprint/extractManagedLabels with bogus values and break
// ticket matching, causing duplicate tickets. The real, sync-managed block
// is always the last one in the description.
func findMetadataBlockStart(description string) int {
	header := metadataHeader
	last := -1
	for i := 0; i <= len(description)-len(header); {
		idx := strings.Index(description[i:], header)
		if idx < 0 {
			break
		}
		absIdx := i + idx
		if absIdx == 0 || description[absIdx-1] == '\n' {
			last = absIdx
		}
		i = absIdx + 1
	}
	return last
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

// resolveManagedLabel resolves one configured label name to its Linear label
// ID. It serves every label this tool applies — the managed set and the
// create-only set alike — so its errors name the label configuration as a whole
// rather than any single environment variable.
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

	// labelRef pairs a resolved label ID with the ID of its exclusive group (or
	// "" when ungrouped), so the group can be recorded alongside the ID and used
	// later to keep at most one child of a group in an issueUpdate's labelIds.
	type labelRef struct {
		id    string
		group string
	}
	var after *string
	var teamMatches []labelRef
	var globalMatches []labelRef
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
      parent {
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
					Parent *struct {
						ID string `json:"id"`
					} `json:"parent"`
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
			ref := labelRef{id: label.ID}
			if label.Parent != nil {
				ref.group = label.Parent.ID
			}
			switch {
			case label.Team != nil && label.Team.ID == c.teamID():
				teamMatches = append(teamMatches, ref)
			case label.Team == nil:
				globalMatches = append(globalMatches, ref)
			}
		}

		if !resp.IssueLabels.PageInfo.HasNextPage || resp.IssueLabels.PageInfo.EndCursor == nil {
			break
		}
		after = resp.IssueLabels.PageInfo.EndCursor
	}

	var resolved labelRef
	switch {
	case len(teamMatches) == 1:
		resolved = teamMatches[0]
	case len(teamMatches) > 1:
		return fmt.Errorf("Linear label %q is ambiguous for team %s; keep only one matching label", managedLabel, c.teamRef())
	case len(globalMatches) == 1:
		resolved = globalMatches[0]
	case len(globalMatches) > 1:
		return fmt.Errorf("Linear label %q is ambiguous across workspace labels; keep only one matching label", managedLabel)
	default:
		return fmt.Errorf("Linear label %q was not found; create the label in Linear, or remove it from the label configuration (LINEAR_MANAGED_LABEL, LINEAR_TOOL_LABELS, LINEAR_ORIGIN_LABELS, LINEAR_AWAITING_FIX_LABEL, LINEAR_CREATE_ONLY_LABELS)", managedLabel)
	}

	c.mu.Lock()
	c.managedLabelIDs[model.NormalizeLabelName(managedLabel)] = resolved.id
	c.managedLabelGroupIDs[model.NormalizeLabelName(managedLabel)] = resolved.group
	c.mu.Unlock()
	return nil
}

// createLabelIDs resolves the labels stamped on a brand-new issue: the managed
// set plus any create-only labels. This is the only place a create-only or
// protected label is ever written — updates never touch them.
func (c *Client) createLabelIDs(desired model.DesiredIssue) []string {
	c.mu.RLock()
	defer c.mu.RUnlock()

	names := createLabelNames(desired)
	out := make([]string, 0, len(names))
	seen := make(map[string]struct{}, len(names))
	for _, label := range names {
		id := c.managedLabelIDs[label]
		if id == "" {
			continue
		}
		if _, exists := seen[id]; exists {
			continue
		}
		out = append(out, id)
		seen[id] = struct{}{}
	}
	return out
}

// desiredLabelIDs computes the full label set to write for an update, since
// Linear's issueUpdate replaces the ticket's labels wholesale rather than
// applying a delta.
//
// The sync owns only its own managed labels. Every other label on the ticket —
// protected, human-applied, or from another automation — is carried forward
// untouched. `live` is the ticket's label set read immediately before the
// write, and is the authority for what the ticket carries right now: the
// `existing` snapshot was taken at the top of the run, so any label added since
// then is absent from it and echoing that stale set back to Linear would delete
// the label. When `live` is nil (direct callers that do not read live labels)
// the snapshot's labels are used as the fallback.
//
// Reconciliation is scoped to the managed set alone: a previously-managed label
// no longer desired is dropped, the desired managed set is (re-)asserted, and
// nothing else is added or removed.
//
// Linear label groups are mutually exclusive: an issue may carry at most one
// child of a group, and issueUpdate rejects a labelIds set that names two
// ("labelIds not exclusive child labels"). Carrying every non-managed label
// forward can put a second child of a group into the set — a human-applied or
// stale sibling alongside the child the sync asserts, or two siblings that
// predate the group being made exclusive. The set is therefore collapsed to at
// most one child per group. A managed label the sync asserts wins the group
// (its severity/tool/origin child replaces whatever sibling was carried
// forward); between two carried-forward siblings the first read wins.
func (c *Client) desiredLabelIDs(existing model.ExistingIssue, desired model.DesiredIssue, live []model.IssueLabel) ([]string, error) {
	desiredManaged := normalizeManagedLabelNames(desired.ManagedLabels)
	protected := c.protectedLabelNames()

	// The live read is the current truth; fall back to the snapshot only when a
	// caller did not supply one (nil, not merely empty — an issue whose labels
	// were all removed reads back as a non-nil empty slice, and must not fall
	// back to the stale snapshot).
	current := live
	if current == nil {
		current = existing.Labels
	}

	out := make([]string, 0, len(current)+len(desiredManaged))
	seen := make(map[string]struct{}, len(current)+len(desiredManaged))
	// groupToIdx maps an exclusive group's ID to the index in `out` of the one
	// child currently kept for it, so a later member of the same group can be
	// dropped (or, for a managed label, replace the kept sibling in place).
	groupToIdx := make(map[string]int, len(current)+len(desiredManaged))
	previousManaged := make(map[string]struct{}, len(existing.ManagedLabels))
	for _, label := range normalizeManagedLabelNames(existing.ManagedLabels) {
		previousManaged[label] = struct{}{}
	}

	// addLabel adds one label ID to the result unless it would be the second
	// child of an exclusive group. group is the label's group ID ("" when
	// ungrouped). A managed label evicts an already-kept sibling of its group; a
	// non-managed one yields to whatever is already kept.
	addLabel := func(id, group string, managed bool) {
		if id == "" {
			return
		}
		if _, exists := seen[id]; exists {
			return
		}
		if group != "" {
			if idx, clash := groupToIdx[group]; clash {
				if managed {
					delete(seen, out[idx])
					out[idx] = id
					seen[id] = struct{}{}
				}
				return
			}
			groupToIdx[group] = len(out)
		}
		out = append(out, id)
		seen[id] = struct{}{}
	}

	// Carry forward every current label the sync does not own. A previously
	// managed label is dropped here and only re-added below if still desired,
	// which is how the sync reconciles its own set; everything else — protected
	// control labels, human labels, other automations' labels — is preserved.
	for _, label := range current {
		normalized := model.NormalizeLabelName(label.Name)
		if _, managed := previousManaged[normalized]; managed {
			continue
		}
		addLabel(label.ID, label.GroupID, false)
	}

	if len(desiredManaged) == 0 {
		return out, nil
	}

	c.mu.RLock()
	defer c.mu.RUnlock()
	for _, managedLabel := range desiredManaged {
		// A protected label must never be asserted from the managed set — that
		// would let a misconfiguration stamp a terminal kikimora gate onto a
		// ticket that does not have one. It is already carried forward above if
		// the ticket genuinely has it.
		if _, isProtected := protected[managedLabel]; isProtected {
			continue
		}
		managedLabelID := c.managedLabelIDs[managedLabel]
		if managedLabelID == "" {
			return nil, fmt.Errorf("managed Linear label %q was not resolved", managedLabel)
		}
		addLabel(managedLabelID, c.managedLabelGroupIDs[managedLabel], true)
	}
	return out, nil
}

// protectedLabelNames returns the configured protected labels as a normalized
// set for membership tests.
func (c *Client) protectedLabelNames() map[string]struct{} {
	out := make(map[string]struct{}, len(c.cfg.Labels.Protected))
	for _, label := range c.cfg.Labels.Protected {
		if normalized := model.NormalizeLabelName(label); normalized != "" {
			out[normalized] = struct{}{}
		}
	}
	return out
}

// fetchLabelsByIssueID reads the current label set of each given issue. It runs
// immediately before an update batch so labels the sync does not own are
// preserved against their live state rather than the run's opening snapshot —
// the window between the two is minutes wide and is exactly when another
// actor's label edit gets clobbered.
//
// The `id in` filter can match at most len(issueIDs) issues, so a single page of
// that size returns all of them and no pagination is needed. This assumes update
// batches stay well under Linear's per-page ceiling, which they do at the
// caller's batch size of 10; a batch larger than the ceiling would fail the
// request, which degrades to the caller's per-issue retry rather than silently
// dropping labels.
func (c *Client) fetchLabelsByIssueID(ctx context.Context, issueIDs []string) (map[string][]model.IssueLabel, error) {
	if len(issueIDs) == 0 {
		return nil, nil
	}

	op := gqlclient.NewOperation(`
query issueLabelsByID($filter: IssueFilter!, $first: Int!) {
  issues(filter: $filter, first: $first, includeArchived: true) {
    nodes {
      id
      labels(first: 250) {
        nodes {
          id
          name
          parent {
            id
          }
        }
      }
    }
  }
}`)
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
						ID     string `json:"id"`
						Name   string `json:"name"`
						Parent *struct {
							ID string `json:"id"`
						} `json:"parent"`
					} `json:"nodes"`
				} `json:"labels"`
			} `json:"nodes"`
		} `json:"issues"`
	}
	if err := c.execute(ctx, op, &resp); err != nil {
		return nil, fmt.Errorf("fetch live Linear labels: %w", err)
	}

	out := make(map[string][]model.IssueLabel, len(resp.Issues.Nodes))
	for _, node := range resp.Issues.Nodes {
		labels := make([]model.IssueLabel, 0, len(node.Labels.Nodes))
		for _, label := range node.Labels.Nodes {
			groupID := ""
			if label.Parent != nil {
				groupID = label.Parent.ID
			}
			labels = append(labels, model.IssueLabel{ID: label.ID, Name: label.Name, GroupID: groupID})
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

// createLabelNames returns every label name to stamp on a new issue: the
// managed set followed by the create-only set. Create-only labels resolve
// through the same lookup as managed ones, so a create-only label that does not
// exist in Linear fails the run loudly instead of being silently dropped.
func createLabelNames(desired model.DesiredIssue) []string {
	names := make([]string, 0, len(desired.ManagedLabels)+len(desired.CreateOnlyLabels))
	names = append(names, desired.ManagedLabels...)
	names = append(names, desired.CreateOnlyLabels...)
	return normalizeManagedLabelNames(names)
}

func managedLabelsFromDesiredIssues(desired []model.DesiredIssue) []string {
	out := make([]string, 0, len(desired))
	for _, issue := range desired {
		out = append(out, createLabelNames(issue)...)
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

func timelessDatePtr(value string) *linearapi.TimelessDate {
	if strings.TrimSpace(value) == "" {
		return nil
	}
	date := linearapi.TimelessDate(value)
	return &date
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
// Snyk finding state that drove it. It returns an empty string when there is
// nothing meaningful to report.
func buildChangeComment(update model.IssueUpdate) string {
	if update.Diff == nil {
		return ""
	}
	d := update.Diff
	// A change confined to the hidden metadata block (e.g. the one-time
	// identity backfill) is still written, but nobody needs a comment on it.
	descriptionChanged := d.DescriptionChanged && !d.MetadataOnlyDescriptionChange
	if !d.TitleChanged && !descriptionChanged && !d.DueDateChanged &&
		!d.StateChanged && !d.PriorityChanged && len(d.LabelsAdded) == 0 && len(d.LabelsRemoved) == 0 {
		return ""
	}

	lines := []string{"**snyk-linear-sync**", ""}

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

	if descriptionChanged {
		lines = append(lines, "- Description updated — Snyk finding data changed")
	}

	if d.TitleChanged {
		lines = append(lines, "- Title updated — Snyk finding data changed")
	}

	if d.PriorityChanged {
		lines = append(lines, fmt.Sprintf("- Priority set to **%s** — Snyk severity changed", priorityName(d.PriorityTo)))
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
