package config

import (
	"os"
	"path/filepath"
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
