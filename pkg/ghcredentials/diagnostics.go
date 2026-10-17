package ghcredentials

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/devsy-org/devsy/pkg/version"
)

type hostDecision struct {
	Host   string `json:"host"`
	Source string `json:"source"`
}

// Only these source-allowlisted fields may enter local diagnostics. Command
// arguments, config contents, accounts, error text, and token values are never
// recorded. gh owns target selection, so its arguments need no diagnostic copy.
type diagnostic struct {
	Version       string             `json:"version"`
	Started       time.Time          `json:"started"`
	Finished      time.Time          `json:"finished,omitzero"`
	Complete      bool               `json:"complete"`
	Binary        string             `json:"binary"`
	ConfigDir     string             `json:"config_dir,omitempty"`
	Inputs        map[string]string  `json:"inputs"`
	Hosts         []hostDecision     `json:"hosts,omitempty"`
	Stage         string             `json:"stage"`
	Code          int                `json:"exit_code"`
	Signal        int                `json:"signal,omitempty"`
	HostStatus    string             `json:"host_status,omitempty"`
	HostContext   HostContext        `json:"host_context"`
	HostDecisions []hostDecision     `json:"host_decisions,omitempty"`
	Operations    []operation        `json:"operations,omitempty"`
	Writeback     *writebackSummary  `json:"writeback,omitempty"`
	Recovery      []recoveryDecision `json:"recovery,omitempty"`
	path          string
}

func beginDiagnostic(binary string) *diagnostic {
	d := &diagnostic{
		Version: version.GetVersion(),
		Binary:  binary,
		Started: time.Now(),
		Stage:   "started",
	}
	if os.Getenv("DEVSY_GH_DIAGNOSTICS") == "false" {
		return d
	}
	root, err := sessionRoot()
	if err != nil {
		return d
	}
	dir := filepath.Join(root, "diagnostics")
	if os.MkdirAll(dir, 0o700) != nil {
		return d
	}
	f, err := os.CreateTemp(dir, "run-*.json")
	if err != nil {
		return d
	}
	d.path = f.Name()
	_ = f.Close()
	d.Inputs = credentialInputs()
	d.save(false)
	return d
}

func (d *diagnostic) run(binary string, args, env []string, owner *os.File) int {
	started := time.Now()
	result := runGH(binary, args, env, owner)
	d.Code = result.Code
	d.Signal = result.Signal
	if d.path != "" {
		op := operation{
			Name:       "gh.execute",
			Status:     "ok",
			DurationMS: time.Since(started).Milliseconds(),
			ErrorType:  result.ErrorType,
		}
		if result.Code != 0 {
			op.Status = "error"
		}
		if result.Signal != 0 {
			op.Status = "cancelled"
		}
		d.Operations = append(d.Operations, op)
	}
	return d.Code
}

func (d *diagnostic) save(complete bool) {
	if d.path == "" {
		return
	}
	d.Complete = complete
	b, err := json.Marshal(d)
	if err == nil {
		_ = os.WriteFile(d.path, b, 0o600)
	}
}

func (d *diagnostic) finish() {
	if d.path == "" {
		return
	}
	d.Finished = time.Now()
	d.save(true)
	entries, _ := os.ReadDir(filepath.Dir(d.path))
	// Fixed-size records and a count bound cap local disk usage. Incomplete
	// records identify killed runs; no remote diagnostic export exists.
	sort.Slice(entries, func(i, j int) bool {
		a, b := entries[i], entries[j]
		successA := diagnosticSuccess(filepath.Join(filepath.Dir(d.path), a.Name()))
		successB := diagnosticSuccess(filepath.Join(filepath.Dir(d.path), b.Name()))
		if successA != successB {
			return successA
		}
		ai, _ := a.Info()
		bi, _ := b.Info()
		return ai != nil && bi != nil && ai.ModTime().Before(bi.ModTime())
	})
	for len(entries) > 64 {
		_ = os.Remove(filepath.Join(filepath.Dir(d.path), entries[0].Name()))
		entries = entries[1:]
	}
}

func diagnosticSuccess(path string) bool {
	b, err := readFile(path)
	if err != nil {
		return false
	}
	var d diagnostic
	if json.Unmarshal(b, &d) != nil {
		return false
	}
	return d.Complete && d.Code == 0 && d.Stage != "writeback_pending"
}

type operation struct {
	Name       string `json:"name"`
	Status     string `json:"status"`
	ErrorType  string `json:"error_type,omitempty"`
	DurationMS int64  `json:"duration_ms"`
}

func (d *diagnostic) observe(name string, started time.Time, err error) {
	if d.path == "" {
		return
	}
	op := operation{Name: name, Status: "ok", DurationMS: time.Since(started).Milliseconds()}
	if err != nil {
		op.Status = "error"
		op.Name, op.ErrorType = describeError(name, err)
	}
	d.Operations = append(d.Operations, op)
}

func credentialInputs() map[string]string {
	inputs := map[string]string{}
	for _, name := range []string{
		"GH_TOKEN", "GITHUB_TOKEN", "GH_ENTERPRISE_TOKEN", "GITHUB_ENTERPRISE_TOKEN",
		"GH_HOST", "GH_CONFIG_DIR", "XDG_CONFIG_HOME", "HOME", "APPDATA",
	} {
		value, present := os.LookupEnv(name)
		state := "unset"
		if present {
			state = "empty"
			if value != "" {
				state = "set"
			}
		}
		inputs[name] = state
	}
	return inputs
}

func (d *diagnostic) hostResult(result HostResult) {
	if d.path == "" || result.Status == "" {
		return
	}
	op := operation{Name: "host.resolve", Status: "ok", DurationMS: result.DurationMS}
	if result.Status != "available" {
		op.Status = "unavailable"
		op.ErrorType = result.Status
	}
	d.Operations = append(d.Operations, op)
}
