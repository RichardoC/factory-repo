package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadDefaultsAndValidation(t *testing.T) {
	t.Setenv("SNYK_CLIENT_ID", "client-id")
	t.Setenv("SNYK_CLIENT_SECRET", "client-secret")
	t.Setenv("SNYK_ORG_ID", "org-id")
	t.Setenv("LINEAR_API_KEY", "linear-key")
	t.Setenv("LINEAR_TEAM_ID", "team-id")
	t.Setenv("SOURCE_PROVIDER", "")
	t.Setenv("LINEAR_MANAGED_LABEL", "")
	t.Setenv("LINEAR_TOOL_LABELS", "")
	t.Setenv("LINEAR_TOOL_LABEL_DEFAULT", "")
	t.Setenv("LINEAR_ORIGIN_LABELS", "")
	t.Setenv("LINEAR_ORIGIN_LABEL_DEFAULT", "")

	cfg, err := Load(nil)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}

	if cfg.Snyk.Region != defaultSnykRegion {
		t.Fatalf("Region = %q, want %q", cfg.Snyk.Region, defaultSnykRegion)
	}
	if cfg.Source.Provider != defaultSourceProvider {
		t.Fatalf("Source provider = %q, want %q", cfg.Source.Provider, defaultSourceProvider)
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
	if cfg.Linear.Labels.ToolDefault != defaultManagedLabel {
		t.Fatalf("Tool default label = %q, want %q", cfg.Linear.Labels.ToolDefault, defaultManagedLabel)
	}
	if len(cfg.Linear.Labels.Tool) != 0 {
		t.Fatalf("Tool labels = %#v, want empty", cfg.Linear.Labels.Tool)
	}
	if cfg.Linear.Labels.OriginDefault != "" {
		t.Fatalf("Origin default label = %q, want empty", cfg.Linear.Labels.OriginDefault)
	}
	if len(cfg.Linear.Labels.Origin) != 0 {
		t.Fatalf("Origin labels = %#v, want empty", cfg.Linear.Labels.Origin)
	}
	if cfg.Linear.Labels.AwaitingFix != defaultAwaitingFixLabel {
		t.Fatalf("AwaitingFix label = %q, want %q", cfg.Linear.Labels.AwaitingFix, defaultAwaitingFixLabel)
	}
	if cfg.Linear.Due.CriticalDays != defaultCriticalDueDays {
		t.Fatalf("Critical due days = %d, want %d", cfg.Linear.Due.CriticalDays, defaultCriticalDueDays)
	}
	if cfg.Linear.Due.HighDays != defaultHighDueDays {
		t.Fatalf("High due days = %d, want %d", cfg.Linear.Due.HighDays, defaultHighDueDays)
	}
	if cfg.Linear.Due.MediumDays != defaultMediumDueDays {
		t.Fatalf("Medium due days = %d, want %d", cfg.Linear.Due.MediumDays, defaultMediumDueDays)
	}
	if cfg.Linear.Due.LowDays != defaultLowDueDays {
		t.Fatalf("Low due days = %d, want %d", cfg.Linear.Due.LowDays, defaultLowDueDays)
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
		"SNYK_CLIENT_ID",
		"SNYK_CLIENT_SECRET",
		"SNYK_ORG_ID",
		"LINEAR_API_KEY",
		"LINEAR_TEAM_ID",
	} {
		t.Setenv(key, "")
	}

	if _, err := Load(nil); err == nil {
		t.Fatal("Load() error = nil, want validation error")
	}
}

func TestSplitCSV(t *testing.T) {
	got := splitCSV("  a, b ,,c ")
	want := []string{"a", "b", "c"}
	if len(got) != len(want) {
		t.Fatalf("splitCSV() len = %d, want %d", len(got), len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("splitCSV()[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

func TestLoadEnvFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, ".env")
	content := "export SNYK_CLIENT_ID='client-id'\n" +
		"SNYK_CLIENT_SECRET=\"client-secret\"\n" +
		"SNYK_ORG_ID=org-id # comment\n" +
		"LINEAR_API_KEY=linear-key\n" +
		"LINEAR_TEAM_ID=team-id\n" +
		"SOURCE_PROVIDER=github\n" +
		"LINEAR_MANAGED_LABEL=off\n" +
		"LINEAR_UNSUBSCRIBE_ACTOR=true\n" +
		"LINEAR_TOOL_LABELS=code:snyk-code, license:snyk-license\n" +
		"LINEAR_TOOL_LABEL_DEFAULT=off\n" +
		"LINEAR_ORIGIN_LABELS=github:snyk-github,kubernetes:snyk-kubernetes\n" +
		"LINEAR_ORIGIN_LABEL_DEFAULT=off\n" +
		"LINEAR_DUE_DAYS_CRITICAL=20\n" +
		"SNYK_OAUTH_SCOPES='scope-a, scope-b'\n"
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}

	cfg, err := Load([]string{"--env-file", path})
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}

	if cfg.Snyk.ClientID != "client-id" {
		t.Fatalf("ClientID = %q, want %q", cfg.Snyk.ClientID, "client-id")
	}
	if cfg.Snyk.ClientSecret != "client-secret" {
		t.Fatalf("ClientSecret = %q, want %q", cfg.Snyk.ClientSecret, "client-secret")
	}
	if cfg.Linear.APIKey != "linear-key" {
		t.Fatalf("APIKey = %q, want %q", cfg.Linear.APIKey, "linear-key")
	}
	if cfg.Linear.TeamID != "team-id" {
		t.Fatalf("TeamID = %q, want %q", cfg.Linear.TeamID, "team-id")
	}
	if cfg.Source.Provider != "github" {
		t.Fatalf("Source provider = %q, want %q", cfg.Source.Provider, "github")
	}
	if cfg.Linear.Labels.Managed != "" {
		t.Fatalf("Managed label = %q, want empty", cfg.Linear.Labels.Managed)
	}
	if !cfg.Linear.UnsubscribeActor {
		t.Fatal("UnsubscribeActor = false, want true")
	}
	if cfg.Linear.Labels.ToolDefault != "" {
		t.Fatalf("Tool default label = %q, want empty", cfg.Linear.Labels.ToolDefault)
	}
	if cfg.Linear.Labels.Tool["code"] != "snyk-code" || cfg.Linear.Labels.Tool["license"] != "snyk-license" {
		t.Fatalf("Tool labels = %#v, want code/license mappings", cfg.Linear.Labels.Tool)
	}
	if cfg.Linear.Labels.OriginDefault != "" {
		t.Fatalf("Origin default label = %q, want empty", cfg.Linear.Labels.OriginDefault)
	}
	if cfg.Linear.Labels.Origin["github"] != "snyk-github" || cfg.Linear.Labels.Origin["kubernetes"] != "snyk-kubernetes" {
		t.Fatalf("Origin labels = %#v, want github/kubernetes mappings", cfg.Linear.Labels.Origin)
	}
	if cfg.Linear.Due.CriticalDays != 20 {
		t.Fatalf("Critical due days = %d, want %d", cfg.Linear.Due.CriticalDays, 20)
	}
	if len(cfg.Snyk.Scopes) != 2 || cfg.Snyk.Scopes[0] != "scope-a" || cfg.Snyk.Scopes[1] != "scope-b" {
		t.Fatalf("Scopes = %#v, want [scope-a scope-b]", cfg.Snyk.Scopes)
	}
}

func TestLoadRejectsMalformedToolLabels(t *testing.T) {
	t.Setenv("SNYK_CLIENT_ID", "client-id")
	t.Setenv("SNYK_CLIENT_SECRET", "client-secret")
	t.Setenv("SNYK_ORG_ID", "org-id")
	t.Setenv("LINEAR_API_KEY", "linear-key")
	t.Setenv("LINEAR_TEAM_ID", "team-id")
	t.Setenv("LINEAR_TOOL_LABELS", "code")

	if _, err := Load(nil); err == nil {
		t.Fatal("Load() error = nil, want parse error")
	}
}

func TestLoadRejectsMalformedOriginLabels(t *testing.T) {
	t.Setenv("SNYK_CLIENT_ID", "client-id")
	t.Setenv("SNYK_CLIENT_SECRET", "client-secret")
	t.Setenv("SNYK_ORG_ID", "org-id")
	t.Setenv("LINEAR_API_KEY", "linear-key")
	t.Setenv("LINEAR_TEAM_ID", "team-id")
	t.Setenv("LINEAR_ORIGIN_LABELS", "github")

	if _, err := Load(nil); err == nil {
		t.Fatal("Load() error = nil, want parse error")
	}
}

// TestLoadProtectsKikimoraLabelsByDefault pins the protected set to the three
// kikimora Dark Factory control labels. Protection is on by default, not
// opt-in: an operator who forgets an env var must not end up with a sync that
// deletes another system's dispatch state.
func TestLoadProtectsKikimoraLabelsByDefault(t *testing.T) {
	setRequiredEnv(t)

	cfg, err := Load(nil)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}

	want := []string{"df:kikimora-snyk", "df:kikimora-snyk-complete", "df:kikimora-snyk-invalid"}
	if len(cfg.Linear.Labels.Protected) != len(want) {
		t.Fatalf("Protected = %#v, want %#v", cfg.Linear.Labels.Protected, want)
	}
	for i, label := range want {
		if cfg.Linear.Labels.Protected[i] != label {
			t.Fatalf("Protected[%d] = %q, want %q", i, cfg.Linear.Labels.Protected[i], label)
		}
	}
}

func TestLoadCreateOnlyLabels(t *testing.T) {
	setRequiredEnv(t)
	t.Setenv("LINEAR_CREATE_ONLY_LABELS", " df:kikimora-snyk , ,off,another-label ")

	cfg, err := Load(nil)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}

	// Blank entries and the "off" disable word are dropped; the rest survive
	// with surrounding whitespace trimmed.
	want := []string{"df:kikimora-snyk", "another-label"}
	if len(cfg.Linear.Labels.CreateOnly) != len(want) {
		t.Fatalf("CreateOnly = %#v, want %#v", cfg.Linear.Labels.CreateOnly, want)
	}
	for i, label := range want {
		if cfg.Linear.Labels.CreateOnly[i] != label {
			t.Fatalf("CreateOnly[%d] = %q, want %q", i, cfg.Linear.Labels.CreateOnly[i], label)
		}
	}
}

func TestLoadCreateOnlyLabelsDefaultsToNone(t *testing.T) {
	setRequiredEnv(t)
	t.Setenv("LINEAR_CREATE_ONLY_LABELS", "")

	cfg, err := Load(nil)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if len(cfg.Linear.Labels.CreateOnly) != 0 {
		t.Fatalf("CreateOnly = %#v, want none by default", cfg.Linear.Labels.CreateOnly)
	}
}

// TestLoadProtectedLabelsCanBeDisabled keeps the escape hatch working for a
// deployment with no downstream consumer of the labels.
func TestLoadProtectedLabelsCanBeDisabled(t *testing.T) {
	setRequiredEnv(t)
	t.Setenv("LINEAR_PROTECTED_LABELS", "off")

	cfg, err := Load(nil)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if len(cfg.Linear.Labels.Protected) != 0 {
		t.Fatalf("Protected = %#v, want none when disabled", cfg.Linear.Labels.Protected)
	}
}

func setRequiredEnv(t *testing.T) {
	t.Helper()
	t.Setenv("SNYK_CLIENT_ID", "client-id")
	t.Setenv("SNYK_CLIENT_SECRET", "client-secret")
	t.Setenv("SNYK_ORG_ID", "org-id")
	t.Setenv("LINEAR_API_KEY", "linear-key")
	t.Setenv("LINEAR_TEAM_ID", "team-id")
}
