package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"slices"
	"strings"
	"syscall"
	"time"

	"github.com/tesslio/snyk-dast-linear-sync/internal/cache"
	"github.com/tesslio/snyk-dast-linear-sync/internal/config"
	linearclient "github.com/tesslio/snyk-dast-linear-sync/internal/linear"
	"github.com/tesslio/snyk-dast-linear-sync/internal/logx"
	"github.com/tesslio/snyk-dast-linear-sync/internal/model"
	snykdastclient "github.com/tesslio/snyk-dast-linear-sync/internal/snykdast"
	syncsvc "github.com/tesslio/snyk-dast-linear-sync/internal/sync"
)

func main() {
	var (
		envFile  = flag.String("env-file", "", "load configuration from a dotenv-style file before reading the process environment")
		issueRef = flag.String("issue", "", "Linear issue identifier (e.g. PROB-12127) or full Linear URL")
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

	// As in the sync binary: Ctrl-C or SIGTERM cancels in-flight calls instead
	// of killing the process mid-write, so the tool reports on what it had
	// loaded rather than dying with the signal. The shutdown line is emitted
	// from finish, on the way out, together with the exit status it announces;
	// nothing here may print it earlier, because until the diagnostic path is
	// done the process has not decided to exit yet.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	snykdast, err := snykdastclient.New(ctx, cfg, logger.With("service", "snyk-dast"))
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: create Snyk DAST client: %v\n", err)
		os.Exit(1)
	}

	cacheStore, cacheErr := cache.Open(cfg.Cache.DBFile)
	if cacheErr == nil {
		defer cacheStore.Close()
	}

	linear := linearclient.New(cfg.Linear, cfg.Sync.LinearConcurrency, logger.With("service", "linear"))

	fmt.Fprintf(os.Stderr, "loading Linear issue %q...\n", identifier)
	existing, err := linear.LoadIssueByIdentifier(ctx, identifier)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: load Linear issue %q: %v\n", identifier, err)
		os.Exit(finish(ctx, logger, 1))
	}

	if existing.Fingerprint == "" {
		fmt.Fprintf(os.Stderr, "error: Linear issue %q does not contain a snyk-dast-linear-sync fingerprint\n", identifier)
		os.Exit(1)
	}

	fmt.Fprintf(os.Stderr, "loading Snyk DAST snapshot for fingerprint %q...\n", existing.Fingerprint)
	snykdastSnapshot, err := snykdast.LoadSnapshot(ctx)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: load Snyk DAST snapshot: %v\n", err)
		os.Exit(finish(ctx, logger, 1))
	}

	finding, findingErr := findFinding(snykdastSnapshot, existing.Fingerprint)
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
	} else {
		printDiagnostics(diag, cacheSnapshot, cacheErr)
	}

	// The diagnostics above are printed either way, so an interrupted run still
	// reports what it saw. This has to be the last thing main does: os.Exit
	// skips deferred calls.
	os.Exit(finish(ctx, logger, exitCode(ctx, nil)))
}

// finish reports the outcome and returns the exit status main passes to
// os.Exit. It is the only place that announces the shutdown, and it does so on
// the way out, next to the exit status the line talks about -- a line printed
// when the signal arrives would describe a decision the process had not made
// yet (the diagnostic path has to run first, and it may still fail).
//
// WHAT the two lines promise, exactly: the shutdown line is printed whenever
// the context was cancelled, whatever the exit status is, and the exit line is
// printed whenever the status is non-zero, whatever the reason (a failed load,
// not only an interruption). finish is not reached at all for the failures that
// happen before the clients are up -- config, flags, client construction -- but
// those cannot have a cancellation to report either: they exit before the
// signal watcher context is created. Every later failure path goes through it.
func finish(ctx context.Context, logger *slog.Logger, status int) int {
	if ctx.Err() != nil {
		logger.Info("shutdown requested; cancelling in-flight calls and exiting non-zero")
	}
	if status != 0 {
		logger.Error("exiting non-zero", "exit_code", status)
	}
	return status
}

// exitCode is the process exit status: non-zero once the context was cancelled.
//
// It keys on the context and not only on the error, because the cancellation
// can land after the last call has already completed successfully -- the call
// returns nil, the run was still interrupted, and the snapshot it printed is
// the one the shutdown log line promised not to trust. A scheduler has to see
// that as a failure, which is what the log line says happens.
func exitCode(ctx context.Context, err error) int {
	if err != nil || ctx.Err() != nil {
		return 1
	}
	return 0
}

func extractIdentifier(ref string) string {
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return ""
	}
	if !strings.Contains(ref, "/") {
		return ref
	}

	// Full URL: https://linear.app/tessl/issue/PROB-12127/snyk-dast-...
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

func findFinding(snapshot model.SnykDASTSnapshot, fingerprint string) (*model.Finding, error) {
	for i := range snapshot.Findings {
		if snapshot.Findings[i].Fingerprint == fingerprint {
			return &snapshot.Findings[i], nil
		}
	}

	targetID, ok := syncsvc.FingerprintTargetID(fingerprint)
	if !ok {
		return nil, fmt.Errorf("Snyk DAST finding %q not found in current snapshot", fingerprint)
	}
	if _, inactive := snapshot.InactiveTargetIDs[targetID]; inactive {
		return nil, fmt.Errorf("Snyk DAST finding %q not found; target %q is inactive", fingerprint, targetID)
	}
	if _, active := snapshot.TargetIDs[targetID]; !active {
		return nil, fmt.Errorf("Snyk DAST finding %q not found; target %q no longer exists", fingerprint, targetID)
	}
	return nil, fmt.Errorf("Snyk DAST finding %q not found in current snapshot", fingerprint)
}

func emitJSON(diag syncsvc.DueDateDiagnostics, cacheSnapshot cache.Snapshot, cacheErr error) {
	cachedSnykDASTHash := cacheSnapshot.SnykDASTHashes[diag.Finding.Fingerprint]
	cachedLinearHash := cacheSnapshot.LinearHashes[diag.Finding.Fingerprint]

	out := struct {
		Diagnostics        syncsvc.DueDateDiagnostics `json:"diagnostics"`
		Cache              cacheEntrySummary          `json:"cache"`
		CacheError         string                     `json:"cache_error,omitempty"`
		CacheSnykDASTMatch bool                       `json:"cache_snyk_dast_match"`
		CacheLinearMatch   bool                       `json:"cache_linear_match"`
	}{
		Diagnostics:        diag,
		Cache:              cacheEntrySummary{SchemaSignature: cacheSnapshot.SchemaSignature, SnykDASTHash: cachedSnykDASTHash, LinearHash: cachedLinearHash},
		CacheSnykDASTMatch: cachedSnykDASTHash == diag.SnykHash,
		CacheLinearMatch:   cachedLinearHash == diag.LinearHash,
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
	SnykDASTHash    string `json:"snyk_dast_hash"`
	LinearHash      string `json:"linear_hash"`
}

func printDiagnostics(diag syncsvc.DueDateDiagnostics, cacheSnapshot cache.Snapshot, cacheErr error) {
	fmt.Println()
	fmt.Printf("Linear issue: %s\n", diag.Existing.Identifier)
	fmt.Printf("URL:          %s\n", diag.Existing.URL)
	fmt.Printf("Fingerprint:  %s\n", diag.Finding.Fingerprint)
	fmt.Printf("Snyk DAST finding ID: %s\n", diag.Finding.SnykDASTFindingID)
	fmt.Println()

	fmt.Println("Linear current state:")
	fmt.Printf("  Due date:       %s\n", emptyIf(diag.Existing.DueDate, "(none)"))
	fmt.Printf("  State:          %s\n", diag.Existing.StateName)
	fmt.Printf("  Priority:       %d\n", diag.Existing.Priority)
	fmt.Printf("  Managed labels: %s\n", strings.Join(diag.Existing.ManagedLabels, ", "))
	fmt.Println()

	fmt.Println("Snyk DAST finding:")
	fmt.Printf("  Status:           %s\n", diag.Finding.Status)
	fmt.Printf("  Severity:         %s\n", diag.Finding.Severity)
	fmt.Printf("  Target:           %s (%s)\n", diag.Finding.TargetName, diag.Finding.TargetID)
	fmt.Printf("  Vulnerable URL:   %s\n", diag.Finding.FindingURL)
	fmt.Printf("  Method/Path:      %s %s\n", diag.Finding.Method, diag.Finding.Path)
	fmt.Printf("  Created at:       %s\n", formatTime(diag.Finding.CreatedAt))
	fmt.Printf("  Acceptance expiry: %s\n", formatTime(diag.Finding.IgnoreExpiresAt))
	fmt.Println()

	fmt.Println("Due date scenarios:")
	for _, s := range diag.Scenarios {
		fmt.Printf("  - %-50s %s (base %s, %s)\n", s.Name+":", s.DueDate, s.Base, s.Reason)
	}
	fmt.Println()

	fmt.Println("Sync decision:")
	fmt.Printf("  Was snoozed:                %v\n", diag.WasSnoozed)
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
		fmt.Printf("  Snyk DAST hash matches: %v\n", cacheSnapshot.SnykDASTHashes[diag.Finding.Fingerprint] == diag.SnykHash)
		fmt.Printf("  Linear hash matches:  %v\n", cacheSnapshot.LinearHashes[diag.Finding.Fingerprint] == diag.LinearHash)
	}
	fmt.Printf("  Snyk DAST hash: %s\n", diag.SnykHash)
	fmt.Printf("  Linear hash:  %s\n", diag.LinearHash)
	fmt.Println()

	if diag.WouldUpdate {
		fmt.Println("Conclusion: the sync would UPDATE this issue on the next run.")
	} else {
		fmt.Println("Conclusion: the sync would NOT update this issue on the next run.")
	}
	if cacheSnapshot.SchemaSignature != "" &&
		cacheSnapshot.SnykDASTHashes[diag.Finding.Fingerprint] == diag.SnykHash &&
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
