package snyk

import (
	"encoding/json"
	"net/url"
	"testing"
	"time"

	"github.com/tesslio/snyk-linear-sync/internal/model"
)

// TestFindingFromIssueDecodesRealIssueShape is the decode-layer guard that
// the unit tests for the renderer skip: it unmarshals a Snyk REST issue
// response fixture (mirroring the real `GET orgs/{org}/issues` resource for
// API version 2024-10-15) into issueResource and asserts that findingFromIssue
// populates every field this sync renders.
//
// The fixture intentionally encodes the gotchas that broke the first
// implementation of issue #26:
//
//   - CVSS lives in attributes.severities[].score, NOT attributes.cvss (which
//     does not exist). CVEs are reported by NVD, so problems[].source is
//     "NVD", not "CVE".
//   - classes entries have no title field (only id/source/type/url).
//   - remediation prose lives in coordinates[].remedies[].description, NOT
//     attributes.remediation (which does not exist).
//   - coordinates carry all six is_fixable_* / is_*able flags, including
//     is_pinnable and is_upgradeable.
func TestFindingFromIssueDecodesRealIssueShape(t *testing.T) {
	const fixture = `{
  "id": "87c4c069-ebb3-43ab-9e65-eb4772263e1c",
  "type": "issue",
  "attributes": {
    "created_at": "2025-09-14T05:50:26.527Z",
    "updated_at": "2025-09-14T05:50:26.527Z",
    "effective_severity_level": "critical",
    "ignored": false,
    "status": "open",
    "title": "Malicious Package",
    "key": "SNYK-JS-DEBUG-12552895",
    "type": "package_vulnerability",
    "exploit_details": {
      "maturity_levels": [
        {"format": "CVSSv3", "level": "High"}
      ]
    },
    "problems": [
      {"id": "SNYK-JS-DEBUG-12552895", "source": "SNYK", "type": "vulnerability"},
      {"id": "CVE-2025-7783", "source": "NVD", "type": "vulnerability"},
      {"id": "CVE-2025-12816", "source": "NVD", "type": "vulnerability"}
    ],
    "classes": [
      {"id": "CWE-506", "source": "CWE", "type": "weakness"}
    ],
    "severities": [
      {"source": "Snyk", "score": 9.3, "level": "critical", "version": "4.0"},
      {"source": "NVD", "score": 7.5, "level": "high", "version": "3.1"}
    ],
    "description": "Prototype pollution allows an attacker to merge recursive objects.",
    "coordinates": [
      {
        "is_fixable_manually": false,
        "is_fixable_snyk": false,
        "is_fixable_upstream": false,
        "is_patchable": false,
        "is_pinnable": true,
        "is_upgradeable": true,
        "state": "open",
        "remedies": [
          {"type": "indeterminate", "description": "Upgrade debug to 4.4.4 or higher.", "details": {"upgrade_package": "4.4.4"}}
        ],
        "representations": [
          {
            "dependency": {"package_name": "debug", "package_version": "4.4.3"},
            "sourceLocation": {
              "commit_id": "abc123",
              "file": "package.json",
              "region": {"start": {"line": 10, "column": 2}, "end": {"line": 12, "column": 8}}
            }
          }
        ]
      }
    ],
    "resolution": {"type": "", "details": ""}
  },
  "relationships": {
    "scan_item": {"data": {"id": "project-a", "type": "project"}}
  }
}`

	var issue issueResource
	if err := json.Unmarshal([]byte(fixture), &issue); err != nil {
		t.Fatalf("unmarshal issue fixture: %v", err)
	}

	// Sanity-check the decode itself so a struct-tag regression is caught
	// independently of the finding mapping.
	if issue.ID != "87c4c069-ebb3-43ab-9e65-eb4772263e1c" {
		t.Fatalf("issue.ID = %q", issue.ID)
	}
	if len(issue.Attributes.Severities) != 2 {
		t.Fatalf("severities len = %d, want 2 (decode must read the new field)", len(issue.Attributes.Severities))
	}
	if issue.Attributes.Coordinates[0].IsPinnable != true || issue.Attributes.Coordinates[0].IsUpgradeable != true {
		t.Fatal("is_pinnable / is_upgradeable did not decode")
	}

	c := &Client{
		restBase: mustParseURL(t, "https://api.snyk.io/rest/"),
	}
	project := projectRef{
		ID:              "project-a",
		Name:            "owner/repo(main):package-lock.json",
		Origin:          "github",
		TargetReference: "main",
		TargetFile:      "package-lock.json",
		Repository:      "owner/repo",
		Active:          true,
	}
	createdAt, err := time.Parse(time.RFC3339, "2025-09-14T05:50:26.527Z")
	if err != nil {
		t.Fatalf("parse created_at: %v", err)
	}

	finding := c.findingFromIssue(issue, "project-a", project, "", "myorg", "SNYK-JS-DEBUG-12552895", "SNYK-JS-DEBUG-12552895", createdAt, time.Time{}, ignoreMetadata{})

	// CVSS: Snyk score (9.3) must win over NVD (7.5) per selectCVSS (Snyk > Red Hat > NVD).
	if finding.CVSS != 9.3 {
		t.Fatalf("CVSS = %v, want 9.3 (selected from Snyk severities, not attributes.cvss)", finding.CVSS)
	}
	// CVEs: source is NVD, but the id prefix is CVE-.
	wantCVEs := []string{"CVE-2025-7783", "CVE-2025-12816"}
	if len(finding.CVEs) != len(wantCVEs) {
		t.Fatalf("CVEs = %#v, want %#v (id-prefix match, source-agnostic)", finding.CVEs, wantCVEs)
	}
	for i := range wantCVEs {
		if finding.CVEs[i] != wantCVEs[i] {
			t.Fatalf("CVEs[%d] = %q, want %q", i, finding.CVEs[i], wantCVEs[i])
		}
	}
	// CWE classes: id present, no title field anywhere.
	if len(finding.Classes) != 1 || finding.Classes[0].ID != "CWE-506" || finding.Classes[0].Source != "CWE" {
		t.Fatalf("Classes = %#v, want [{ID:CWE-506 Source:CWE}]", finding.Classes)
	}
	// Description decoded from attributes.description.
	if finding.Description != "Prototype pollution allows an attacker to merge recursive objects." {
		t.Fatalf("Description = %q", finding.Description)
	}
	// Remediation decoded from coordinates[].remedies[].description.
	if finding.Remediation != "Upgrade debug to 4.4.4 or higher." {
		t.Fatalf("Remediation = %q, want remedy prose from coordinates", finding.Remediation)
	}
	// Fixability flags: all six, including the two previously-missing ones.
	if !finding.HasCoordinates {
		t.Fatal("HasCoordinates = false, want true")
	}
	if finding.IsPinnable != true {
		t.Fatal("IsPinnable = false, want true (previously dropped by missing flag)")
	}
	if finding.IsUpgradeable != true {
		t.Fatal("IsUpgradeable = false, want true (previously dropped by missing flag)")
	}
	// FixedVersion still sourced from remedies[].details.upgrade_package.
	if finding.FixedVersion != "4.4.4" {
		t.Fatalf("FixedVersion = %q, want 4.4.4", finding.FixedVersion)
	}
	// Package and source location.
	if finding.PackageName != "debug" {
		t.Fatalf("PackageName = %q, want debug", finding.PackageName)
	}
	if finding.SourceFile != "package.json" || finding.SourceCommitID != "abc123" {
		t.Fatalf("source = file=%q commit=%q, want package.json/abc123", finding.SourceFile, finding.SourceCommitID)
	}
	if finding.SourceLineStart != 10 || finding.SourceLineEnd != 12 {
		t.Fatalf("source region = %d-%d, want 10-12", finding.SourceLineStart, finding.SourceLineEnd)
	}
	// Identity + status.
	if finding.SnykIssueID != "87c4c069-ebb3-43ab-9e65-eb4772263e1c" {
		t.Fatalf("SnykIssueID = %q", finding.SnykIssueID)
	}
	if finding.SnykIssueKey != "SNYK-JS-DEBUG-12552895" {
		t.Fatalf("SnykIssueKey = %q", finding.SnykIssueKey)
	}
	if finding.Status != model.FindingOpen {
		t.Fatalf("Status = %q, want %q", finding.Status, model.FindingOpen)
	}
	// URLs populated.
	if finding.IssueURL == "" || finding.IssueAPIURL == "" {
		t.Fatalf("URLs empty: ui=%q api=%q", finding.IssueURL, finding.IssueAPIURL)
	}
}

// TestFindingFromIssueCodeFindingRemediationProse verifies the code/cloud
// finding shape, where remedies[].description is the primary remediation
// surface and there are no CVE/severities entries at all.
func TestFindingFromIssueCodeFindingRemediationProse(t *testing.T) {
	const fixture = `{
  "id": "issue-code-1",
  "type": "issue",
  "attributes": {
    "created_at": "2026-01-01T00:00:00Z",
    "effective_severity_level": "high",
    "ignored": false,
    "status": "open",
    "title": "Path Traversal",
    "key": "SNYK-CODE-1",
    "type": "code",
    "problems": [{"id": "SNYK-CODE-1", "source": "SNYK"}],
    "classes": [{"id": "CWE-22", "source": "CWE"}],
    "coordinates": [
      {
        "is_fixable_manually": true,
        "is_fixable_snyk": false,
        "is_fixable_upstream": false,
        "is_patchable": false,
        "is_pinnable": false,
        "is_upgradeable": false,
        "remedies": [
          {"type": "rule_result_message", "description": "Validate user input before passing to file APIs."}
        ],
        "representations": [
          {"sourceLocation": {"file": "src/io.go", "region": {"start": {"line": 42}, "end": {"line": 42}}}}
        ]
      }
    ],
    "resolution": {"type": "", "details": ""}
  },
  "relationships": {"scan_item": {"data": {"id": "project-code", "type": "project"}}}
}`

	var issue issueResource
	if err := json.Unmarshal([]byte(fixture), &issue); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	c := &Client{restBase: mustParseURL(t, "https://api.snyk.io/rest/")}
	createdAt, _ := time.Parse(time.RFC3339, "2026-01-01T00:00:00Z")
	finding := c.findingFromIssue(issue, "project-code", projectRef{ID: "project-code", Name: "owner/repo"}, "", "myorg", "SNYK-CODE-1", "SNYK-CODE-1", createdAt, time.Time{}, ignoreMetadata{})

	if finding.CVSS != 0 {
		t.Fatalf("CVSS = %v, want 0 when no severities reported (code finding)", finding.CVSS)
	}
	if len(finding.CVEs) != 0 {
		t.Fatalf("CVEs = %#v, want empty", finding.CVEs)
	}
	if finding.Remediation != "Validate user input before passing to file APIs." {
		t.Fatalf("Remediation = %q", finding.Remediation)
	}
	if !finding.IsFixableManually || finding.IsUpgradeable || finding.IsPinnable {
		t.Fatalf("fix flags = manual=%v pin=%v upgrade=%v, want manual=true pin=false upgrade=false", finding.IsFixableManually, finding.IsPinnable, finding.IsUpgradeable)
	}
	if finding.SourceFile != "src/io.go" || finding.SourceLineStart != 42 {
		t.Fatalf("source = %q:%d, want src/io.go:42", finding.SourceFile, finding.SourceLineStart)
	}
}

// TestFindingFromIssueMapsKubernetesContext verifies that the cluster passed
// in from the v1 project detail and the namespace parsed into the projectRef
// land on the resulting Finding for kubernetes-origin projects, and that
// non-kubernetes findings stay empty.
func TestFindingFromIssueMapsKubernetesContext(t *testing.T) {
	issue := issueResource{
		ID: "issue-1",
		Attributes: issueAttributes{
			CreatedAt: "2026-01-01T00:00:00Z",
			Status:    "open",
			Title:     "CVE-2026-63075",
			Type:      "package_vulnerability",
		},
	}
	c := &Client{restBase: mustParseURL(t, "https://api.snyk.io/rest/")}
	createdAt, _ := time.Parse(time.RFC3339, "2026-01-01T00:00:00Z")

	k8s := c.findingFromIssue(issue, "project-k8s", projectRef{
		ID:        "project-k8s",
		Name:      "backend/deployment.apps/backend:/app/package.json",
		Origin:    "kubernetes",
		Namespace: "backend",
		Active:    true,
	}, "production", "myorg", "SNYK-1", "SNYK-1", createdAt, time.Time{}, ignoreMetadata{})
	if k8s.ProjectCluster != "production" {
		t.Fatalf("ProjectCluster = %q, want production", k8s.ProjectCluster)
	}
	if k8s.ProjectNamespace != "backend" {
		t.Fatalf("ProjectNamespace = %q, want backend", k8s.ProjectNamespace)
	}

	gh := c.findingFromIssue(issue, "project-gh", projectRef{
		ID:     "project-gh",
		Name:   "owner/repo",
		Origin: "github",
		Active: true,
	}, "", "myorg", "SNYK-1", "SNYK-1", createdAt, time.Time{}, ignoreMetadata{})
	if gh.ProjectCluster != "" || gh.ProjectNamespace != "" {
		t.Fatalf("non-kubernetes finding got cluster=%q namespace=%q, want both empty", gh.ProjectCluster, gh.ProjectNamespace)
	}
}

func mustParseURL(t *testing.T, raw string) *url.URL {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatalf("parse %q: %v", raw, err)
	}
	return u
}
