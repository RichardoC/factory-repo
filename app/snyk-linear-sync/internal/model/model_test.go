package model

import "testing"

func TestFingerprint(t *testing.T) {
	tests := []struct {
		name        string
		projectID   string
		issueID     string
		locationKey string
		want        string
	}{
		{"coarse (no location)", "proj-a", "issue-1", "", "snyk:proj-a:issue-1"},
		{"code location", "proj-a", "issue-1", "e2e/prerequisite_gate.py", "snyk:proj-a:issue-1:e2e/prerequisite_gate.py"},
		{"dependency location", "proj-a", "issue-1", "lodash@4.17.21", "snyk:proj-a:issue-1:lodash@4.17.21"},
		{"empty issueID with location", "proj-a", "", "file.py", "snyk:proj-a::file.py"},
		{"dunder path kept canonical", "proj-a", "issue-1", "sim/__main__.py", "snyk:proj-a:issue-1:sim/__main__.py"},
		{"asterisks canonicalized on construction", "proj-a", "issue-1", "sim/**main**.py", "snyk:proj-a:issue-1:sim/__main__.py"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := Fingerprint(tt.projectID, tt.issueID, tt.locationKey)
			if got != tt.want {
				t.Fatalf("Fingerprint(%q, %q, %q) = %q, want %q", tt.projectID, tt.issueID, tt.locationKey, got, tt.want)
			}
		})
	}
}

// TestCanonicalFingerprint covers the markdown rewrites Linear's editor
// applies to stored descriptions: underscore emphasis re-serialized as
// asterisks and backslash escapes added before punctuation. Verified against
// the live Linear API (2026-07-16): a description written with
// "sim/__main__.py" inside the metadata comment is returned as
// "sim/**main**.py".
func TestCanonicalFingerprint(t *testing.T) {
	tests := []struct {
		name string
		fp   string
		want string
	}{
		{"bold rewrite (observed live)", "snyk:proj-a:issue-1:sim/**main**.py", "snyk:proj-a:issue-1:sim/__main__.py"},
		{"italic rewrite", "snyk:proj-a:issue-1:pkg/*internal*/file.py", "snyk:proj-a:issue-1:pkg/_internal_/file.py"},
		{"backslash escapes stripped", `snyk:proj-a:issue-1:routes/\[orgId\]/get.test.ts`, "snyk:proj-a:issue-1:routes/[orgId]/get.test.ts"},
		{"already canonical is unchanged", "snyk:proj-a:issue-1:sim/__main__.py", "snyk:proj-a:issue-1:sim/__main__.py"},
		{"tilde version untouched", "snyk:proj-a:issue-1:systemd/libsystemd0@257.13-1~deb13u1", "snyk:proj-a:issue-1:systemd/libsystemd0@257.13-1~deb13u1"},
		{"idempotent", "snyk:proj-a:issue-1:sim/__main__.py", "snyk:proj-a:issue-1:sim/__main__.py"},
		{"empty", "", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := CanonicalFingerprint(tt.fp)
			if got != tt.want {
				t.Fatalf("CanonicalFingerprint(%q) = %q, want %q", tt.fp, got, tt.want)
			}
			if again := CanonicalFingerprint(got); again != got {
				t.Fatalf("CanonicalFingerprint not idempotent: %q -> %q", got, again)
			}
		})
	}
}

func TestCoarseFingerprint(t *testing.T) {
	tests := []struct {
		name string
		fp   string
		want string
	}{
		{"fine-grained (code)", "snyk:proj-a:issue-1:e2e/prerequisite_gate.py", "snyk:proj-a:issue-1"},
		{"fine-grained (dep)", "snyk:proj-a:issue-1:lodash@4.17.21", "snyk:proj-a:issue-1"},
		{"already coarse", "snyk:proj-a:issue-1", "snyk:proj-a:issue-1"},
		{"no prefix", "random-string", "random-string"},
		{"empty", "", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := CoarseFingerprint(tt.fp)
			if got != tt.want {
				t.Fatalf("CoarseFingerprint(%q) = %q, want %q", tt.fp, got, tt.want)
			}
		})
	}
}

func TestFingerprintLocation(t *testing.T) {
	for fp, want := range map[string]string{
		"snyk:proj:issue":                 "",
		"snyk:proj:issue:pkg@1.0.0":       "pkg@1.0.0",
		"snyk:proj:issue:dir/a:b.py":      "dir/a:b.py",
		"not-a-fingerprint":               "",
		"snyk:proj":                       "",
		"snyk:proj:issue:sim/__main__.py": "sim/__main__.py",
	} {
		if got := FingerprintLocation(fp); got != want {
			t.Errorf("FingerprintLocation(%q) = %q, want %q", fp, got, want)
		}
	}
}

func TestFindingIdentity(t *testing.T) {
	base := Finding{
		Fingerprint:       Fingerprint("proj-old", "issue-old", "glibc@2.41-12"),
		SnykIssueID:       "issue-old",
		SnykIssueKey:      "SNYK-DEBIAN13-GLIBC-19383357",
		ProjectID:         "proj-old",
		ProjectName:       "tesslio/monorepo(main):harness/kikimora/Dockerfile.simulators",
		ProjectOrigin:     "github",
		ProjectReference:  "main",
		ProjectTargetFile: "harness/kikimora/Dockerfile.simulators",
	}
	identity := FindingIdentity(base)
	if len(identity) != identityLength {
		t.Fatalf("identity %q has length %d, want %d", identity, len(identity), identityLength)
	}

	recreated := base
	recreated.ProjectID = "proj-new"
	recreated.SnykIssueID = "issue-new"
	recreated.Fingerprint = Fingerprint("proj-new", "issue-new", "glibc@2.41-12")
	if got := FindingIdentity(recreated); got != identity {
		t.Fatalf("identity changed across project recreation: %q vs %q", got, identity)
	}

	for name, mutate := range map[string]func(*Finding){
		"origin":     func(f *Finding) { f.ProjectOrigin = "gitlab" },
		"name":       func(f *Finding) { f.ProjectName = "other" },
		"targetFile": func(f *Finding) { f.ProjectTargetFile = "Dockerfile" },
		"reference":  func(f *Finding) { f.ProjectReference = "dev" },
		"repository": func(f *Finding) { f.Repository = "other/repo" },
		"cluster":    func(f *Finding) { f.ProjectCluster = "prod" },
		"issueKey":   func(f *Finding) { f.SnykIssueKey = "SNYK-OTHER" },
		"location":   func(f *Finding) { f.Fingerprint = Fingerprint("proj-old", "issue-old", "glibc@2.42") },
	} {
		changed := base
		mutate(&changed)
		if FindingIdentity(changed) == identity {
			t.Errorf("identity ignores %s", name)
		}
	}

	unknownCluster := base
	unknownCluster.ProjectClusterUnknown = true
	if FindingIdentity(unknownCluster) != "" {
		t.Fatalf("identity must be empty when the cluster lookup failed")
	}

	missingKey := base
	missingKey.SnykIssueKey = ""
	missingName := base
	missingName.ProjectName = " "
	if FindingIdentity(missingKey) != "" || FindingIdentity(missingName) != "" {
		t.Fatalf("identity must be empty without a project name and issue key")
	}
}
