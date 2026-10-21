// routinebrief.go is the run side of an agent routine (issue #117): when the
// routines workflow dispatches a CodeAF run with routine=<name>, `harness
// routine-brief NAME` builds that run's brief. A routine run is read-only,
// like a plan run: no patch, no branch, no PR — its output is proposals in
// the planner's format, which the publish job opens as codeaf-proposed
// issues. The brief is built from the routine's own template under
// .github/codeaf/routines/ (trusted config), wrapped in the same budget
// line and output contract the plan brief uses, and capped like every other
// brief. Standard library only.
package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// runRoutineBrief implements `harness routine-brief NAME`, in the run job
// with MODE=routine. Exactly like runPlanBrief, only two things fail it: a
// missing BRIEF_PATH and an unreadable routine config or template — both
// are installation errors, since routines.json and the templates are part
// of this module. Everything the run gathers from the API is the template's
// business, never this command's.
func runRoutineBrief() error {
	if len(os.Args) != 3 {
		return errors.New("usage: harness routine-brief NAME")
	}
	briefPath := os.Getenv("BRIEF_PATH")
	if briefPath == "" {
		return errors.New("BRIEF_PATH is empty")
	}
	rts, err := LoadRoutines()
	if err != nil {
		return err
	}
	var rt *routine
	for i := range rts {
		if rts[i].Name == os.Args[2] {
			rt = &rts[i]
			break
		}
	}
	if rt == nil {
		return fmt.Errorf("routine-brief: %q is not a routine in routines.json", os.Args[2])
	}
	if rt.Kind != routineKindAgent {
		return fmt.Errorf("routine-brief: %s is a %s routine; a brief template exists only for agent routines", rt.Name, rt.Kind)
	}
	// The template path was validated on load (routineBriefRe: relative,
	// no "..", no leading slash) to stay under routines/; reading it with
	// the same join keeps the containment in one place.
	tmpl, err := os.ReadFile(filepath.Join(harnessDir(), filepath.FromSlash(rt.Brief)))
	if err != nil {
		return err
	}
	parts := []string{budgetLine(), string(tmpl), planOutputContract()}
	brief := capBrief(strings.Join(parts, "\n\n")+"\n", maxBriefBytes)
	fmt.Print(brief)
	if err := os.MkdirAll(filepath.Dir(briefPath), 0o755); err != nil {
		return err
	}
	return os.WriteFile(briefPath, []byte(brief), 0o644)
}
