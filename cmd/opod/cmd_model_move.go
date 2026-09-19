package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"
)

// moveArgs is `opod model move <id> --from <node> --to <node>` parsed.
type moveArgs struct {
	ID, From, To string
}

// parseModelMove reads the move's arguments; the error is the usage problem.
func parseModelMove(args []string) (moveArgs, error) {
	var m moveArgs
	for i := 0; i < len(args); i++ {
		a := args[i]
		name, value, inline := strings.Cut(a, "=")
		switch name {
		case "--from", "--to":
			if !inline {
				if i+1 >= len(args) {
					return m, fmt.Errorf("%s needs a node id", name)
				}
				i++
				value = args[i]
			}
			if value == "" {
				return m, fmt.Errorf("%s needs a node id", name)
			}
			if name == "--from" {
				m.From = value
			} else {
				m.To = value
			}
		default:
			if strings.HasPrefix(a, "-") {
				return m, fmt.Errorf("unknown flag %q", a)
			}
			if m.ID != "" {
				return m, fmt.Errorf("one model at a time: got %q and %q", m.ID, a)
			}
			m.ID = a
		}
	}
	if m.ID == "" || m.From == "" || m.To == "" {
		return m, fmt.Errorf("a model id, --from and --to are all required")
	}
	return m, nil
}

// moveAnswer is the admin route's answer: the steps of a finished move, or
// the error and the steps of an aborted one.
type moveAnswer struct {
	Steps []struct {
		Step string         `json:"step"`
		At   time.Time      `json:"at"`
		Data map[string]any `json:"data"`
	} `json:"steps"`
	Note  string `json:"note"`
	Error struct {
		Message string `json:"message"`
	} `json:"error"`
}

// modelMove asks the running leader to move a model between two workers. A
// move has no store-only path, unlike `node drain`: it is the leader's router
// that stops choosing the source and the leader's counters that say the
// source is idle, so with no leader answering there is nothing to do it.
func modelMove(m moveArgs) {
	cfg := loadConfigOrExit()
	body, _ := json.Marshal(map[string]string{"from": m.From, "to": m.To})
	note(os.Stdout, "moving %s from %s to %s: the target loads it while %s keeps serving (a cold target downloads the weights first — this can take a while)…", m.ID, m.From, m.To, m.From)
	raw, err := adminCallT(context.Background(), cfg, "POST", "/admin/v1/models/"+m.ID+"/move", body, weightsOpTimeout)
	var ans moveAnswer
	_ = json.Unmarshal(raw, &ans)
	for _, s := range ans.Steps {
		fmt.Printf("  %s  %-8s %s\n", s.At.Local().Format("15:04:05"), s.Step, moveStepText(s.Step, s.Data))
	}
	if err != nil {
		if len(raw) == 0 {
			die("%v\n  a move needs the running leader — start it with `opod up` on this host", err)
		}
		if ans.Error.Message != "" {
			die("%s", ans.Error.Message)
		}
		die("%v: %s", err, strings.TrimSpace(string(raw)))
	}
	ok(os.Stdout, "%s is now served by %s; %s let it go", m.ID, m.To, m.From)
	if ans.Note != "" {
		note(os.Stdout, "%s", ans.Note)
	}
	note(os.Stdout, "no KV cache moved: a conversation %s was serving recomputes its prompt on %s at its next turn", m.From, m.To)
}

// moveStepText is one step, in words.
func moveStepText(step string, data map[string]any) string {
	switch step {
	case "started":
		if data["target_already_serves"] == true {
			return "admitted; the target already serves the model, so nothing is loaded"
		}
		return "admitted; loading on the target, the source keeps serving"
	case "loaded":
		return "the target serves the model — both workers take requests"
	case "flipped":
		return "the source is out of rotation: new requests go to the target"
	case "drained":
		if data["timed_out"] == true {
			return fmt.Sprintf("drain timeout: %v request(s) still in flight on the source", data["inflight_left"])
		}
		return "nothing is in flight on the source"
	case "finished":
		return "the source unloaded the model"
	case "aborted":
		return fmt.Sprintf("%v — %v", data["reason"], data["state"])
	}
	return ""
}
