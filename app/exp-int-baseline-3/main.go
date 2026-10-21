// Command jobq is a toy job queue: a dispatcher that starts runs, a gate
// that decides what each dispatch means, and an executor.
//
//	jobq decide EVENT.json    print the gate's decision as key=value lines
//	jobq dispatch CONFIG.json STATE.json
//	                          print one JSON object per run to start
package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
)

func main() {
	if err := run(os.Args[1:], os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, "jobq:", err)
		os.Exit(1)
	}
}

func run(args []string, out io.Writer) error {
	if len(args) == 0 {
		return errors.New("usage: jobq decide|dispatch ...")
	}
	switch args[0] {
	case "decide":
		if len(args) != 2 {
			return errors.New("usage: jobq decide EVENT.json")
		}
		var ev Event
		if err := readJSON(args[1], &ev); err != nil {
			return err
		}
		d := Decide(ev)
		fmt.Fprintf(out, "run=%t\nreason=%s\n", d.Run, d.Reason)
		if d.Run {
			// A digest run has no issue or PR number, so no number= line.
			if d.Mode == "digest" {
				fmt.Fprintf(out, "mode=%s\n", d.Mode)
			} else {
				fmt.Fprintf(out, "mode=%s\nnumber=%d\n", d.Mode, d.Number)
			}
		}
		return nil
	case "dispatch":
		if len(args) != 3 {
			return errors.New("usage: jobq dispatch CONFIG.json STATE.json")
		}
		cfg, err := loadConfig(args[1])
		if err != nil {
			return err
		}
		var st State
		if err := readJSON(args[2], &st); err != nil {
			return err
		}
		enc := json.NewEncoder(out)
		for _, inputs := range Dispatch(cfg, st) {
			if err := enc.Encode(inputs); err != nil {
				return err
			}
		}
		return nil
	}
	return fmt.Errorf("unknown command %q", args[0])
}

func readJSON(path string, v any) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	return json.Unmarshal(data, v)
}
