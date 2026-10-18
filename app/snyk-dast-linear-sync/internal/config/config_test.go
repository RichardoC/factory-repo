package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoadDefaultsAndValidation(t *testing.T) {
	t.Setenv("SNYK_DAST_API_KEY", "api-key")
	t.Setenv("LINEAR_API_KEY", "linear-key")
	t.Setenv("LINEAR_TEAM_ID", "team-id")
	t.Setenv("LINEAR_MANAGED_LABEL", "")
	t.Setenv("LINEAR_TARGET_TYPE_LABELS", "")
	t.Setenv("LINEAR_TARGET_TYPE_LABEL_DEFAULT", "")

	cfg, err := Load(nil)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}

	if cfg.SnykDAST.APIBase != defaultSnykDASTAPIBase {
		t.Fatalf("APIBase = %q, want %q", cfg.SnykDAST.APIBase, defaultSnykDASTAPIBase)
	}
	if cfg.SnykDAST.AppBase != defaultSnykDASTAppBase {
		t.Fatalf("AppBase = %q, want %q", cfg.SnykDAST.AppBase, defaultSnykDASTAppBase)
	}
	if cfg.Linear.States.Todo != defaultLinearTodoState {
		t.Fatalf("Todo state = %q, want %q", cfg.Linear.States.Todo, defaultLinearTodoState)
	}
	if cfg.Linear.Labels.Managed != defaultManagedLabel {
		t.Fatalf("Managed label = %q, want %q", cfg.Linear.Labels.Managed, defaultManagedLabel)
	}
	if !cfg.Linear.UnsubscribeActor {
		t.Fatal("UnsubscribeActor = false, want true")
	}
	if cfg.Linear.Labels.TargetTypeDefault != "" {
		t.Fatalf("Target type default label = %q, want empty", cfg.Linear.Labels.TargetTypeDefault)
	}
	if len(cfg.Linear.Labels.TargetType) != 0 {
		t.Fatalf("Target type labels = %#v, want empty", cfg.Linear.Labels.TargetType)
	}
	if cfg.Linear.Due.CriticalDays != defaultCriticalDueDays {
		t.Fatalf("Critical due days = %d, want %d", cfg.Linear.Due.CriticalDays, defaultCriticalDueDays)
	}
	if cfg.Sync.Workers != defaultWorkerCount {
		t.Fatalf("Workers = %d, want %d", cfg.Sync.Workers, defaultWorkerCount)
	}
	if cfg.Cache.DBFile != defaultCacheDBFile {
		t.Fatalf("Cache DB file = %q, want %q", cfg.Cache.DBFile, defaultCacheDBFile)
	}
	if cfg.Cache.BypassCache {
		t.Fatal("BypassCache = true, want false")
	}
}

func TestLoadRequiresCredentials(t *testing.T) {
	for _, key := range []string{
		"SNYK_DAST_API_KEY",
		"LINEAR_API_KEY",
		"LINEAR_TEAM_ID",
	} {
		t.Setenv(key, "")
	}

	if _, err := Load(nil); err == nil {
		t.Fatal("Load() error = nil, want validation error")
	}
}

func TestLoadEnvFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, ".env")
	content := "export SNYK_DAST_API_KEY='api-key'\n" +
		"LINEAR_API_KEY=linear-key\n" +
		"LINEAR_TEAM_ID=team-id\n" +
		"LINEAR_MANAGED_LABEL=off\n" +
		"LINEAR_UNSUBSCRIBE_ACTOR=true\n" +
		"LINEAR_TARGET_TYPE_LABELS=single:snyk-dast-web,api:snyk-dast-api\n" +
		"LINEAR_TARGET_TYPE_LABEL_DEFAULT=off\n" +
		"LINEAR_DUE_DAYS_CRITICAL=20\n" +
		"SNYK_DAST_TEAM=team-abc\n"
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}

	cfg, err := Load([]string{"--env-file", path})
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}

	if cfg.SnykDAST.APIKey != "api-key" {
		t.Fatalf("APIKey = %q, want %q", cfg.SnykDAST.APIKey, "api-key")
	}
	if cfg.SnykDAST.Team != "team-abc" {
		t.Fatalf("Team = %q, want %q", cfg.SnykDAST.Team, "team-abc")
	}
	if cfg.Linear.APIKey != "linear-key" {
		t.Fatalf("APIKey = %q, want %q", cfg.Linear.APIKey, "linear-key")
	}
	if cfg.Linear.TeamID != "team-id" {
		t.Fatalf("TeamID = %q, want %q", cfg.Linear.TeamID, "team-id")
	}
	if cfg.Linear.Labels.Managed != "" {
		t.Fatalf("Managed label = %q, want empty", cfg.Linear.Labels.Managed)
	}
	if !cfg.Linear.UnsubscribeActor {
		t.Fatal("UnsubscribeActor = false, want true")
	}
	if cfg.Linear.Labels.TargetTypeDefault != "" {
		t.Fatalf("Target type default label = %q, want empty", cfg.Linear.Labels.TargetTypeDefault)
	}
	if cfg.Linear.Labels.TargetType["single"] != "snyk-dast-web" || cfg.Linear.Labels.TargetType["api"] != "snyk-dast-api" {
		t.Fatalf("Target type labels = %#v, want single/api mappings", cfg.Linear.Labels.TargetType)
	}
	if cfg.Linear.Due.CriticalDays != 20 {
		t.Fatalf("Critical due days = %d, want %d", cfg.Linear.Due.CriticalDays, 20)
	}
}

func TestLoadRejectsMalformedTargetTypeLabels(t *testing.T) {
	t.Setenv("SNYK_DAST_API_KEY", "api-key")
	t.Setenv("LINEAR_API_KEY", "linear-key")
	t.Setenv("LINEAR_TEAM_ID", "team-id")
	t.Setenv("LINEAR_TARGET_TYPE_LABELS", "single")

	if _, err := Load(nil); err == nil {
		t.Fatal("Load() error = nil, want parse error")
	}
}

// minimalValidConfig returns a Config that passes Validate, so a test can vary a
// single field and attribute any error to that field.
func minimalValidConfig() Config {
	return Config{
		Log:   LogConfig{ErrorFile: "errors.log"},
		Cache: CacheConfig{DBFile: "cache.db"},
		SnykDAST: SnykDASTConfig{
			APIKey:  "api-key",
			APIBase: defaultSnykDASTAPIBase,
			AppBase: defaultSnykDASTAppBase,
		},
		Linear: LinearConfig{
			APIKey:              "linear-key",
			TeamID:              "team-id",
			ArchiveLookbackDays: defaultArchiveLookbackDays,
			Due: DueDateConfig{
				CriticalDays: defaultCriticalDueDays,
				HighDays:     defaultHighDueDays,
				MediumDays:   defaultMediumDueDays,
				LowDays:      defaultLowDueDays,
			},
		},
		Sync: SyncConfig{
			Workers:             defaultWorkerCount,
			SnykDASTConcurrency: defaultSnykDASTConcurrency,
			LinearConcurrency:   defaultLinearConcurrency,
		},
	}
}

func TestValidateRejectsOverflowingArchiveLookback(t *testing.T) {
	// A day count above ~106751 overflows the nanosecond time.Duration used to
	// compute the archive cutoff, which would silently exclude every archived
	// issue rather than including more of them.
	cases := []struct {
		name    string
		days    int
		wantErr bool
	}{
		{"zero is rejected", 0, true},
		{"negative is rejected", -1, true},
		{"default is accepted", 3650, false},
		{"maximum is accepted", 106751, false},
		{"one past maximum is rejected", 106752, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := minimalValidConfig()
			cfg.Linear.ArchiveLookbackDays = tc.days
			err := cfg.Validate()
			if tc.wantErr && err == nil {
				t.Fatalf("Validate() error = nil, want an error for %d days", tc.days)
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("Validate() error = %v, want nil for %d days", err, tc.days)
			}
		})
	}
}

// TestLoadParsesProtectedLabels pins the parsing of LINEAR_PROTECTED_LABELS:
// comma-separated, whitespace-trimmed, empty entries dropped. The names are
// preserved verbatim (not lower-cased) because they are matched
// case-insensitively at use, but written back to Linear as-is.
func TestLoadParsesProtectedLabels(t *testing.T) {
	t.Setenv("SNYK_DAST_API_KEY", "api-key")
	t.Setenv("LINEAR_API_KEY", "linear-key")
	t.Setenv("LINEAR_TEAM_ID", "team-id")
	t.Setenv("LINEAR_PROTECTED_LABELS", " df:dispatch , ,df:dispatch-complete ")

	cfg, err := Load(nil)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}

	got := cfg.Linear.Labels.Protected
	want := []string{"df:dispatch", "df:dispatch-complete"}
	if len(got) != len(want) {
		t.Fatalf("Protected = %#v, want %#v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("Protected = %#v, want %#v", got, want)
		}
	}
}

// TestLoadDefaultsToNoProtectedLabels keeps the live-label read off the hot path
// unless it is explicitly asked for.
func TestLoadDefaultsToNoProtectedLabels(t *testing.T) {
	t.Setenv("SNYK_DAST_API_KEY", "api-key")
	t.Setenv("LINEAR_API_KEY", "linear-key")
	t.Setenv("LINEAR_TEAM_ID", "team-id")
	t.Setenv("LINEAR_PROTECTED_LABELS", "")

	cfg, err := Load(nil)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if len(cfg.Linear.Labels.Protected) != 0 {
		t.Fatalf("Protected = %#v, want empty by default", cfg.Linear.Labels.Protected)
	}
}

// TestLoadProtectedLabelsCanBeDisabled keeps the escape hatch working for a
// deployment that inherits a protected list from an env file and wants it off
// for one run.
func TestLoadProtectedLabelsCanBeDisabled(t *testing.T) {
	t.Setenv("SNYK_DAST_API_KEY", "api-key")
	t.Setenv("LINEAR_API_KEY", "linear-key")
	t.Setenv("LINEAR_TEAM_ID", "team-id")
	t.Setenv("LINEAR_PROTECTED_LABELS", "off")

	cfg, err := Load(nil)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if len(cfg.Linear.Labels.Protected) != 0 {
		t.Fatalf("Protected = %#v, want empty when disabled with \"off\"", cfg.Linear.Labels.Protected)
	}
}

// setRequiredVars sets the three variables Load requires, with valid values,
// so a test can isolate the malformed variable it actually cares about.
func setRequiredVars(t *testing.T) {
	t.Helper()
	t.Setenv("SNYK_DAST_API_KEY", "api-key")
	t.Setenv("LINEAR_API_KEY", "linear-key")
	t.Setenv("LINEAR_TEAM_ID", "team-id")
}

// TestLoadRejectsMalformedIntegers verifies that a typo'd integer variable
// stops the run with an error naming the variable and echoing the raw value,
// instead of being silently replaced by the default. The archive lookback
// window is the dangerous one: a silent fallback to a too-short window hides
// recently auto-archived tickets and the sync mints duplicates.
func TestLoadRejectsMalformedIntegers(t *testing.T) {
	for _, key := range []string{
		"SYNC_WORKERS",
		"SNYK_DAST_HTTP_CONCURRENCY",
		"LINEAR_HTTP_CONCURRENCY",
		"LINEAR_DUE_DAYS_CRITICAL",
		"LINEAR_DUE_DAYS_HIGH",
		"LINEAR_DUE_DAYS_MEDIUM",
		"LINEAR_DUE_DAYS_LOW",
		"LINEAR_ARCHIVE_LOOKBACK_DAYS",
	} {
		t.Run(key, func(t *testing.T) {
			setRequiredVars(t)
			t.Setenv(key, "1o")

			_, err := Load(nil)
			if err == nil {
				t.Fatalf("Load() error = nil, want error naming %s", key)
			}
			if !strings.Contains(err.Error(), key) {
				t.Fatalf("Load() error = %q, want it to name %s", err, key)
			}
			if !strings.Contains(err.Error(), "1o") {
				t.Fatalf("Load() error = %q, want it to quote the value %q", err, "1o")
			}
		})
	}

	t.Run("reports all malformed integers at once", func(t *testing.T) {
		setRequiredVars(t)
		t.Setenv("LINEAR_ARCHIVE_LOOKBACK_DAYS", "30d")
		t.Setenv("SYNC_WORKERS", "1O")

		_, err := Load(nil)
		if err == nil {
			t.Fatal("Load() error = nil, want an error naming both variables")
		}
		for _, want := range []string{"LINEAR_ARCHIVE_LOOKBACK_DAYS", "30d", "SYNC_WORKERS", "1O"} {
			if !strings.Contains(err.Error(), want) {
				t.Fatalf("Load() error = %q, want it to mention %q", err, want)
			}
		}
	})
}

// TestLoadRejectsMalformedBooleans verifies the same for the two boolean
// variables, which only accept the forms strconv.ParseBool accepts: "yes" and
// "no" are not among them, so they must fail the run rather than flip a
// setting silently.
func TestLoadRejectsMalformedBooleans(t *testing.T) {
	setRequiredVars(t)
	t.Setenv("LINEAR_COMMENTS", "yes")
	t.Setenv("LINEAR_UNSUBSCRIBE_ACTOR", "no")

	_, err := Load(nil)
	if err == nil {
		t.Fatal("Load() error = nil, want an error naming both variables")
	}
	for _, want := range []string{"LINEAR_COMMENTS", "LINEAR_UNSUBSCRIBE_ACTOR"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("Load() error = %q, want it to mention %s", err, want)
		}
	}
}

// TestLoadAcceptsParseBoolSpellings pins the accepted spellings: every form
// strconv.ParseBool accepts is valid, not just "true"/"false".
func TestLoadAcceptsParseBoolSpellings(t *testing.T) {
	for _, tc := range []struct {
		value string
		want  bool
	}{
		{value: "T", want: true},
		{value: "1", want: true},
		{value: "TRUE", want: true},
		{value: "False", want: false},
	} {
		t.Run(tc.value, func(t *testing.T) {
			setRequiredVars(t)
			t.Setenv("LINEAR_COMMENTS", tc.value)

			cfg, err := Load(nil)
			if err != nil {
				t.Fatalf("Load() error = %v", err)
			}
			if cfg.Linear.CommentsEnabled != tc.want {
				t.Fatalf("CommentsEnabled = %v, want %v", cfg.Linear.CommentsEnabled, tc.want)
			}
		})
	}
}

// TestLoadEmptyValuesUseDefaults verifies that unset, empty and whitespace-only
// values still fall back to the default: the change only rejects values that
// are present but do not parse.
func TestLoadEmptyValuesUseDefaults(t *testing.T) {
	setRequiredVars(t)
	t.Setenv("SYNC_WORKERS", "")
	t.Setenv("LINEAR_COMMENTS", "  ")

	cfg, err := Load(nil)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if cfg.Sync.Workers != defaultWorkerCount {
		t.Fatalf("Workers = %d, want %d", cfg.Sync.Workers, defaultWorkerCount)
	}
	if cfg.Linear.CommentsEnabled {
		t.Fatal("CommentsEnabled = true, want the default false")
	}
}

// TestLoadReportsMalformedValuesBeforeMissingCredentials verifies that the
// malformed-value errors come back on their own, before the missing-credential
// validation errors: an operator with a typo should not have to read past a
// wall of "is required" errors to find it.
func TestLoadReportsMalformedValuesBeforeMissingCredentials(t *testing.T) {
	t.Setenv("SNYK_DAST_API_KEY", "")
	t.Setenv("LINEAR_API_KEY", "")
	t.Setenv("LINEAR_TEAM_ID", "")
	t.Setenv("SYNC_WORKERS", "x")

	_, err := Load(nil)
	if err == nil {
		t.Fatal("Load() error = nil, want an error naming SYNC_WORKERS")
	}
	if !strings.Contains(err.Error(), "SYNC_WORKERS") {
		t.Fatalf("Load() error = %q, want it to mention SYNC_WORKERS", err)
	}
	if strings.Contains(err.Error(), "is required") {
		t.Fatalf("Load() error = %q, want malformed values reported before missing credentials", err)
	}
}

// TestLoadEnvFileWithMalformedValue exercises the real flag parsing and
// godotenv.Overload path the binary uses: a malformed integer in the dotenv
// file must fail startup with an error naming the variable and quoting the
// value, not silently fall back to the default.
func TestLoadEnvFileWithMalformedValue(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, ".env")
	content := "SNYK_DAST_API_KEY=api-key\n" +
		"LINEAR_API_KEY=linear-key\n" +
		"LINEAR_TEAM_ID=team-id\n" +
		"LINEAR_DUE_DAYS_HIGH=thirty\n"
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}

	_, err := Load([]string{"--env-file", path})
	if err == nil {
		t.Fatal("Load() error = nil, want an error naming LINEAR_DUE_DAYS_HIGH")
	}
	for _, want := range []string{"LINEAR_DUE_DAYS_HIGH", "thirty"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("Load() error = %q, want it to mention %q", err, want)
		}
	}
}
