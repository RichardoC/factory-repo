package main

import (
	"strings"
	"testing"
)

func TestExperimentProfile(t *testing.T) {
	t.Setenv("HARNESS_DIR", ".")
	t.Setenv("PROXY_MODELS", "models.json")
	p, err := experimentProfile("kimi")
	if err != nil {
		t.Fatal(err)
	}
	if p.Worker != "moonshotai/kimi-k3" || p.Planner != p.Worker || p.Checker != p.Worker {
		t.Fatalf("kimi profile = %+v", p)
	}
	for _, name := range []string{"baseline", "deepseek", "glm53", "kimi"} {
		if _, err := experimentProfile(name); err != nil {
			t.Errorf("committed profile %q: %v", name, err)
		}
	}
	for _, bad := range []string{"", "nope", "Kimi", "../models", "a b", strings.Repeat("a", 33)} {
		if _, err := experimentProfile(bad); err == nil {
			t.Errorf("experimentProfile(%q) accepted", bad)
		}
	}
}

func TestExperimentModelsAreNotLanes(t *testing.T) {
	for _, m := range laneModels(testModels(t)) {
		if m.Role == "experiment" {
			t.Fatalf("lane model %s has role experiment", m.ID)
		}
	}
	only := []Model{{ID: "x", Role: "experiment"}}
	if got := laneModels(only); len(got) != 1 {
		t.Fatalf("laneModels with only experiment models = %v; want them all back", got)
	}
}

func TestSeatConfigProfile(t *testing.T) {
	t.Setenv("HARNESS_DIR", ".")
	t.Setenv("PROXY_MODELS", "models.json")
	out, err := seatConfigStdout(t, "1", "glm53")
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"models.tiers.worker", "models.tiers.mastermind", "models.tiers.high"} {
		if !strings.Contains(out, `"`+key+`": "zai-org/glm-5.3"`) {
			t.Errorf("seat-config with profile glm53: %s not pinned in %s", key, out)
		}
	}
	if _, err := seatConfigStdout(t, "1", "nope"); err == nil {
		t.Error("seat-config accepted an unknown profile")
	}
}

func TestDecideExperiment(t *testing.T) {
	t.Setenv("GITHUB_REPOSITORY", "RichardoC/factory-repo")
	t.Setenv("GITHUB_RUN_ID", "42")
	t.Setenv("HARNESS_DIR", ".")
	t.Setenv("PROXY_MODELS", "models.json")
	exp := item{Number: 3, State: "open", Labels: []label{{Name: "codeaf"}, {Name: experimentLabel}}}
	plain := item{Number: 4, State: "open", Labels: []label{{Name: "codeaf"}}}
	lookupPR := func(int) (item, error) { return item{}, nil }
	dispatch := func(i item, profile string) Decision {
		t.Helper()
		ev := event{Inputs: map[string]string{"issue": "3", "profile": profile}}
		got, err := Decide("workflow_dispatch", ev, "codeaf", lookupPR, func(int) (item, error) { return i, nil })
		if err != nil {
			t.Fatal(err)
		}
		return got
	}
	if got := dispatch(exp, "kimi"); !got.Run || got.Profile != "kimi" {
		t.Fatalf("experiment with profile: %+v", got)
	}
	for _, c := range []struct {
		name    string
		i       item
		profile string
	}{
		{"experiment without profile", exp, ""},
		{"experiment with unknown profile", exp, "nope"},
		{"plain issue with profile", plain, "kimi"},
	} {
		if got := dispatch(c.i, c.profile); got.Run {
			t.Errorf("%s ran: %+v", c.name, got)
		}
	}
	if got := dispatch(plain, ""); !got.Run || got.Profile != "" {
		t.Fatalf("plain dispatch: %+v", got)
	}

	ev := event{Label: &label{Name: "codeaf"}, Issue: &exp}
	if got, _ := Decide("issues", ev, "codeaf", lookupPR, nil); got.Run {
		t.Errorf("labelling an experiment issue ran: %+v", got)
	}
	ev = event{Issue: &exp, Comment: &comment{Body: "/codeaf go", AuthorAssociation: "OWNER", User: user{Login: "RichardoC", Type: "User"}}}
	if got, _ := Decide("issue_comment", ev, "codeaf", lookupPR, nil); got.Run {
		t.Errorf("a command on an experiment issue ran: %+v", got)
	}
}
