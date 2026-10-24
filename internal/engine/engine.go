// Package engine runs tasks on AI coding CLIs (Claude Code, Codex, Gemini CLI, ...) and routes
// each task to the first engine that is installed, signed in and not out of credit.
//
// An engine is described by data, not code: the commands to detect it, check it is signed in,
// run a task, and the patterns that mean "out of usage". Built-ins cover Claude Code, Codex and
// Gemini CLI; a TOML file in the engines directory adds another or overrides one.
package engine

import (
	"embed"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/BurntSushi/toml"
)

// Def describes one engine.
type Def struct {
	Name        string   `toml:"name"`
	Description string   `toml:"description"`
	Detect      string   `toml:"detect"`      // executable that must be on PATH
	Ready       []string `toml:"ready"`       // exits 0 when signed in
	ReadyMatch  string   `toml:"ready_match"` // optional: Ready's output must also match this regexp
	// Run is the task command for owner requests; RunLimited for team requests (no writes, no
	// shell). Placeholders: {prompt}, {dir}, {out}. An engine without RunLimited never takes
	// team tasks.
	Run        []string `toml:"run"`
	RunLimited []string `toml:"run_limited"`
	// Output is "stdout" (the answer is what the command prints) or "file" (it writes {out}).
	Output string `toml:"output"`
	// Exhausted patterns mean "out of usage/credit". A named group (?P<reset>\d{10}) holding a
	// Unix time sets when the engine is usable again; otherwise Cooldown applies.
	Exhausted []string `toml:"exhausted"`
	Cooldown  string   `toml:"cooldown"`

	exhausted []*regexp.Regexp
	ready     *regexp.Regexp
	cooldown  time.Duration
	source    string
}

//go:embed builtin/*.toml
var builtin embed.FS

// Load returns the built-in definitions overlaid with *.toml files from dir (which may not exist).
func Load(dir string) (map[string]*Def, error) {
	defs := map[string]*Def{}
	ents, err := builtin.ReadDir("builtin")
	if err != nil {
		return nil, err
	}
	for _, e := range ents {
		b, err := builtin.ReadFile("builtin/" + e.Name())
		if err != nil {
			return nil, err
		}
		d, err := parse(b, "built-in "+e.Name())
		if err != nil {
			return nil, err
		}
		defs[d.Name] = d
	}
	files, _ := filepath.Glob(filepath.Join(dir, "*.toml"))
	sort.Strings(files)
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			return nil, err
		}
		d, err := parse(b, f)
		if err != nil {
			return nil, err
		}
		defs[d.Name] = d
	}
	return defs, nil
}

func parse(b []byte, source string) (*Def, error) {
	d := &Def{Output: "stdout", Cooldown: "1h", source: source}
	md, err := toml.Decode(string(b), d)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", source, err)
	}
	if un := md.Undecoded(); len(un) > 0 {
		return nil, fmt.Errorf("%s: unknown field %q", source, un[0].String())
	}
	if d.Name == "" || len(d.Run) == 0 {
		return nil, fmt.Errorf("%s: name and run are required", source)
	}
	if d.Detect == "" {
		d.Detect = d.Run[0]
	}
	if d.Output != "stdout" && d.Output != "file" {
		return nil, fmt.Errorf("%s: output must be stdout or file", source)
	}
	if d.cooldown, err = time.ParseDuration(d.Cooldown); err != nil {
		return nil, fmt.Errorf("%s: cooldown: %w", source, err)
	}
	for _, p := range d.Exhausted {
		re, err := regexp.Compile(p)
		if err != nil {
			return nil, fmt.Errorf("%s: exhausted pattern %q: %w", source, p, err)
		}
		d.exhausted = append(d.exhausted, re)
	}
	if d.ReadyMatch != "" {
		if d.ready, err = regexp.Compile(d.ReadyMatch); err != nil {
			return nil, fmt.Errorf("%s: ready_match: %w", source, err)
		}
	}
	return d, nil
}

// Source says where the definition came from.
func (d *Def) Source() string { return d.source }

// command fills the placeholders of a command template.
func command(tmpl []string, prompt, dir, out string) []string {
	r := strings.NewReplacer("{prompt}", prompt, "{dir}", dir, "{out}", out)
	args := make([]string, len(tmpl))
	for i, a := range tmpl {
		if a == "{prompt}" { // the whole argument: no replacer pass over user text
			args[i] = prompt
			continue
		}
		args[i] = r.Replace(a)
	}
	return args
}

// exhaustion reports whether output means the engine is out of usage, and until when if it says.
// Only a failed run, or a short answer that is nothing but the error, counts: a real answer that
// merely mentions rate limits must not take the engine out of rotation.
func (d *Def) exhaustion(output string, failed bool) (bool, time.Time) {
	if !failed && len(strings.TrimSpace(output)) > 400 {
		return false, time.Time{}
	}
	tail := output
	if len(tail) > 8192 {
		tail = tail[len(tail)-8192:]
	}
	for _, re := range d.exhausted {
		m := re.FindStringSubmatch(tail)
		if m == nil {
			continue
		}
		if i := re.SubexpIndex("reset"); i > 0 && m[i] != "" {
			var sec int64
			if _, err := fmt.Sscan(m[i], &sec); err == nil && sec > time.Now().Unix() {
				return true, time.Unix(sec, 0)
			}
		}
		return true, time.Now().Add(d.cooldown)
	}
	return false, time.Time{}
}
