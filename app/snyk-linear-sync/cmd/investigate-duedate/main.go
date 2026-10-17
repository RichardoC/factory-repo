package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/tesslio/snyk-linear-sync/internal/cache"
	"github.com/tesslio/snyk-linear-sync/internal/config"
	linearclient "github.com/tesslio/snyk-linear-sync/internal/linear"
	"github.com/tesslio/snyk-linear-sync/internal/logx"
	"github.com/tesslio/snyk-linear-sync/internal/model"
	snykclient "github.com/tesslio/snyk-linear-sync/internal/snyk"
	syncsvc "github.com/tesslio/snyk-linear-sync/internal/sync"
)

func main() {
	var (
		envFile  = flag.String("env-file", "", "load configuration from a dotenv-style file before reading the process environment")
		issueRef = flag.String("issue", "", "Linear issue identifier (e.g. SNYK-12127) or full Linear URL")
		jsonOut  = flag.Bool("json", false, "emit JSON diagnostics instead of human-readable text")
	)
	flag.Parse()

	if strings.TrimSpace(*issueRef) == "" {
		fmt.Fprintln(os.Stderr, "error: --issue is required")
		flag.Usage()
		os.Exit(1)
	}

	identifier := extractIdentifier(*issueRef)
	if identifier == "" {
		fmt.Fprintf(os.Stderr, "error: could not extract Linear identifier from %q\n", *issueRef)
		os.Exit(1)
	}

	args := []string(nil)
	if strings.TrimSpace(*envFile) != "" {
		args = append(args, "--env-file", strings.TrimSpace(*envFile))
	}
	cfg, err := config.Load(args)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: load config: %v\n", err)
		os.Exit(1)
	}

	logger := slog.New(logx.NewMultiHandler(
		slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}),
	))

	ctx := context.Background()

	snyk, err := snykclient.New(ctx, cfg, logger.With("service", "snyk"))
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: create Snyk client: %v\n", err)
		os.Exit(1)
	}

	cacheStore, cacheErr := cache.Open(cfg.Cache.DBFile)
	if cacheErr == nil {
		defer cacheStore.Close()
		snyk.SetCache(cacheStore)
	}

	linear := linearclient.New(cfg.Linear, cfg.Sync.LinearConcurrency, logger.With("service", "linear"))

	fmt.Fprintf(os.Stderr, "loading Linear issue %q...\n", identifier)
	existing, err := linear.LoadIssueByIdentifier(ctx, identifier)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: load Linear issue %q: %v\n", identifier, err)
		os.Exit(1)
	}

	if existing.Fingerprint == "" {
		fmt.Fprintf(os.Stderr, "error: Linear issue %q does not contain a snyk-linear-sync fingerprint\n", identifier)
		os.Exit(1)
	}

	fmt.Fprintf(os.Stderr, "loading Snyk snapshot for fingerprint %q...\n", existing.Fingerprint)
	snykSnapshot, err := snyk.LoadSnapshot(ctx)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: load Snyk snapshot: %v\n", err)
		os.Exit(1)
	}

	finding, findingErr := findFinding(snykSnapshot, existing.Fingerprint)
	if findingErr != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", findingErr)
		os.Exit(1)
	}

	diag := syncsvc.DiagnoseDueDate(cfg, *finding, existing)

	var cacheSnapshot cache.Snapshot
	if cacheErr == nil {
		cacheSnapshot, cacheErr = cacheStore.Load(ctx)
	}

	if *jsonOut {
		emitJSON(diag, cacheSnapshot, cacheErr)
		return
	}

	printDiagnostics(diag, cacheSnapshot, cacheErr)
}

func extractIdentifier(ref string) string {
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return ""
	}
	if !strings.Contains(ref, "/") {
		return ref
	}

	// Full URL: https://linear.app/tessl/issue/SNYK-12127/snyk-...
	parts := strings.Split(ref, "/")
	for i, part := range parts {
		if part == "issue" && i+1 < len(parts) {
			candidate := parts[i+1]
			if candidate != "" {
				return candidate
			}
		}
	}
	for _, part := range slices.Backward(parts) {
		if part != "" {
			return part
		}
	}
	return ""
}

func findFinding(snapshot model.SnykSnapshot, fingerprint string) (*model.Finding, error) {
	for i := range snapshot.Findings {
		if snapshot.Findings[i].Fingerprint == fingerprint {
			return &snapshot.Findings[i], nil
		}
	}

	projectID, ok := syncsvc.FingerprintProjectID(fingerprint)
	if !ok {
		return nil, fmt.Errorf("Snyk finding %q not found in current snapshot", fingerprint)
	}
	if _, inactive := snapshot.InactiveProjectIDs[projectID]; inactive {
		return nil, fmt.Errorf("Snyk finding %q not found; project %q is inactive", fingerprint, projectID)
	}
	if _, active := snapshot.ProjectIDs[projectID]; !active {
		return nil, fmt.Errorf("Snyk finding %q not found; project %q no longer exists", fingerprint, projectID)
	}
	return nil, fmt.Errorf("Snyk finding %q not found in current snapshot", fingerprint)
}

func emitJSON(diag syncsvc.DueDateDiagnostics, cacheSnapshot cache.Snapshot, cacheErr error) {
	cachedSnykHash := cacheSnapshot.SnykHashes[diag.Finding.Fingerprint]
	cachedLinearHash := cacheSnapshot.LinearHashes[diag.Finding.Fingerprint]

	out := struct {
		Diagnostics      syncsvc.DueDateDiagnostics `json:"diagnostics"`
		Cache            cacheEntrySummary          `json:"cache"`
		CacheError       string                     `json:"cache_error,omitempty"`
		CacheSnykMatch   bool                       `json:"cache_snyk_match"`
		CacheLinearMatch bool                       `json:"cache_linear_match"`
	}{
		Diagnostics:      diag,
		Cache:            cacheEntrySummary{SchemaSignature: cacheSnapshot.SchemaSignature, SnykHash: cachedSnykHash, LinearHash: cachedLinearHash},
		CacheSnykMatch:   cachedSnykHash == diag.SnykHash,
		CacheLinearMatch: cachedLinearHash == diag.LinearHash,
	}
	if cacheErr != nil {
		out.CacheError = cacheErr.Error()
	}
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	if err := enc.Encode(out); err != nil {
		fmt.Fprintf(os.Stderr, "error: encode JSON: %v\n", err)
		os.Exit(1)
	}
}

type cacheEntrySummary struct {
	SchemaSignature string `json:"schema_signature"`
	SnykHash        string `json:"snyk_hash"`
	LinearHash      string `json:"linear_hash"`
}

func printDiagnostics(diag syncsvc.DueDateDiagnostics, cacheSnapshot cache.Snapshot, cacheErr error) {
	fmt.Println()
	fmt.Printf("Linear issue: %s\n", diag.Existing.Identifier)
	fmt.Printf("URL:          %s\n", diag.Existing.URL)
	fmt.Printf("Fingerprint:  %s\n", diag.Finding.Fingerprint)
	fmt.Printf("Snyk issue key: %s\n", diag.Finding.SnykIssueKey)
	fmt.Printf("Snyk issue ID:  %s\n", diag.Finding.SnykIssueID)
	fmt.Println()

	fmt.Println("Linear current state:")
	fmt.Printf("  Due date:       %s\n", emptyIf(diag.Existing.DueDate, "(none)"))
	fmt.Printf("  State:          %s\n", diag.Existing.StateName)
	fmt.Printf("  Priority:       %d\n", diag.Existing.Priority)
	fmt.Printf("  Managed labels: %s\n", strings.Join(diag.Existing.ManagedLabels, ", "))
	fmt.Println()

	fmt.Println("Snyk finding:")
	fmt.Printf("  Status:               %s\n", diag.Finding.Status)
	fmt.Printf("  Severity:             %s\n", diag.Finding.Severity)
	fmt.Printf("  Issue type:           %s\n", diag.Finding.IssueType)
	fmt.Printf("  Project origin:       %s\n", diag.Finding.ProjectOrigin)
	fmt.Printf("  Created at:           %s\n", formatTime(diag.Finding.CreatedAt))
	fmt.Printf("  Ignore expires at:    %s\n", formatTime(diag.Finding.IgnoreExpiresAt))
	fmt.Printf("  Disregard if fixable: %v\n", diag.Finding.DisregardIfFixable)
	fmt.Println()

	fmt.Println("Due date scenarios:")
	for _, s := range diag.Scenarios {
		fmt.Printf("  - %-45s %s (base %s, %s)\n", s.Name+":", s.DueDate, s.Base, s.Reason)
	}
	fmt.Printf("  - %-45s %s (base %s, %s)\n", diag.FixAvailabilityScenario.Name+":", diag.FixAvailabilityScenario.DueDate, diag.FixAvailabilityScenario.Base, diag.FixAvailabilityScenario.Reason)
	fmt.Println()

	fmt.Println("Sync decision:")
	fmt.Printf("  Was awaiting fix:           %v\n", diag.WasAwaitingFix)
	fmt.Printf("  Effective desired due date: %s\n", emptyIf(diag.Desired.DueDate, "(none)"))
	fmt.Printf("  Desired due date base:      %s\n", emptyIf(diag.Desired.DueDateBase, "(none)"))
	fmt.Printf("  Due date reason:            %s\n", diag.Desired.DueDateReason)
	fmt.Printf("  Desired state:              %s\n", diag.Desired.State)
	fmt.Printf("  Would update:               %v\n", diag.WouldUpdate)
	fmt.Printf("  Pending terminal transition: %v\n", diag.PendingTerminalTransition)
	if diag.Diff != nil {
		if diag.Diff.DueDateChanged {
			fmt.Printf("  Diff due date:              %q -> %q\n", diag.Diff.DueDateFrom, diag.Diff.DueDateTo)
		}
		if diag.Diff.StateChanged {
			fmt.Printf("  Diff state:                 %q -> %q\n", diag.Diff.StateFrom, diag.Diff.StateTo)
		}
		if diag.Diff.DescriptionChanged {
			fmt.Println("  Diff description:           changed")
		}
		if diag.Diff.TitleChanged {
			fmt.Printf("  Diff title:                 %q -> %q\n", diag.Diff.TitleFrom, diag.Diff.TitleTo)
		}
	}
	fmt.Println()

	fmt.Println("Cache:")
	if cacheErr != nil {
		fmt.Printf("  Error loading:      %v\n", cacheErr)
	} else if cacheSnapshot.SchemaSignature == "" {
		fmt.Println("  No cache found")
	} else {
		fmt.Printf("  Schema signature:   %s\n", cacheSnapshot.SchemaSignature)
		fmt.Printf("  Snyk hash matches:  %v\n", cacheSnapshot.SnykHashes[diag.Finding.Fingerprint] == diag.SnykHash)
		fmt.Printf("  Linear hash matches: %v\n", cacheSnapshot.LinearHashes[diag.Finding.Fingerprint] == diag.LinearHash)
	}
	fmt.Printf("  Snyk hash:          %s\n", diag.SnykHash)
	fmt.Printf("  Linear hash:        %s\n", diag.LinearHash)
	fmt.Println()

	if diag.WouldUpdate {
		fmt.Println("Conclusion: the sync would UPDATE this issue on the next run.")
	} else {
		fmt.Println("Conclusion: the sync would NOT update this issue on the next run.")
	}
	if cacheSnapshot.SchemaSignature != "" &&
		cacheSnapshot.SnykHashes[diag.Finding.Fingerprint] == diag.SnykHash &&
		cacheSnapshot.LinearHashes[diag.Finding.Fingerprint] == diag.LinearHash {
		fmt.Println("The cache claims the issue is unchanged (this is a cache hit).")
	} else if cacheSnapshot.SchemaSignature != "" {
		fmt.Println("The cache does NOT match the current live state (this is a cache miss).")
	}
}

func emptyIf(value, fallback string) string {
	if value == "" {
		return fallback
	}
	return value
}

func formatTime(t time.Time) string {
	if t.IsZero() {
		return "(none)"
	}
	return t.UTC().Format(time.RFC3339)
}
