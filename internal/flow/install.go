package flow

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"

	"github.com/i7c/ais/internal/ais"
)

// unit is one systemd user unit flow installs.
type unit struct{ name, text string }

// dispatchUnits run "ais flow dispatch" every five minutes. systemd gives
// the rest: a timer firing while its one-shot service still runs is a no-op,
// linger keeps it going without a login, and journalctl has its output.
func dispatchUnits(bin string) []unit {
	env := "Environment=PATH=%h/.local/bin:/usr/local/bin:/usr/bin:/bin\n"
	if h := os.Getenv("AIS_HOME"); h != "" {
		env += "Environment=AIS_HOME=" + h + "\n"
	}
	return []unit{
		{"ais-flow-dispatch.service", `# Written by "ais flow install"; it rewrites this file.
[Unit]
Description=ais flow: start queued sessions within the limits

[Service]
Type=oneshot
` + env + `ExecStart=` + bin + ` flow dispatch
`},
		{"ais-flow-dispatch.timer", `# Written by "ais flow install"; it rewrites this file.
[Unit]
Description=ais flow: dispatch queued sessions every five minutes

[Timer]
OnCalendar=*:0/5
RandomizedDelaySec=20
Persistent=false

[Install]
WantedBy=timers.target
`},
	}
}

// plannerUnits run "ais flow plan <ws>" on each workspace's schedule: one
// template service, one timer per workspace (a timer's OnCalendar cannot
// come from its instance name).
func plannerUnits(bin string, cfg *Config) []unit {
	env := "Environment=PATH=%h/.local/bin:/usr/local/bin:/usr/bin:/bin\n"
	if h := os.Getenv("AIS_HOME"); h != "" {
		env += "Environment=AIS_HOME=" + h + "\n"
	}
	units := []unit{{"ais-flow-plan@.service", `# Written by "ais flow install"; it rewrites this file.
[Unit]
Description=ais flow: start a planner for workspace %i when tickets are ready

[Service]
Type=oneshot
` + env + `ExecStart=` + bin + ` flow plan %i --auto
`}}
	var names []string
	for n := range cfg.Workspaces {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		units = append(units, unit{"ais-flow-plan-" + n + ".timer", `# Written by "ais flow install"; it rewrites this file.
[Unit]
Description=ais flow: planner check for workspace ` + n + `

[Timer]
OnCalendar=` + cfg.Workspaces[n].Planner.Schedule + `
Unit=ais-flow-plan@` + n + `.service
Persistent=false

[Install]
WantedBy=timers.target
`})
	}
	return units
}

func unitDir() string {
	if x := os.Getenv("XDG_CONFIG_HOME"); x != "" {
		return filepath.Join(x, "systemd", "user")
	}
	return filepath.Join(home(), ".config", "systemd", "user")
}

func cmdInstall(args []string) error {
	o, err := parseOpts(args, nil, []string{"--dry-run", "--no-enable"}, map[string]string{"-n": "--dry-run"})
	if err != nil {
		return err
	}
	cfg, err := loadConfig()
	if err != nil {
		return err
	}
	dry := o.flags["--dry-run"]
	units := append(dispatchUnits(ais.Self()), plannerUnits(ais.Self(), cfg)...)
	dir := unitDir()
	changed := false
	for _, u := range units {
		path := filepath.Join(dir, u.name)
		old, _ := os.ReadFile(path)
		action := "wrote"
		switch {
		case string(old) == u.text:
			action = "unchanged"
		case len(old) > 0 && !strings.Contains(string(old), `"ais flow install"`):
			fmt.Fprintf(stdout, "%12s  %s  (not written by ais flow; left alone)\n", "check", path)
			continue
		case len(old) > 0:
			action = "rewrote"
		}
		if action != "unchanged" {
			changed = true
			if dry {
				action = map[string]string{"wrote": "would write", "rewrote": "would rewrite"}[action]
			} else {
				if err := os.MkdirAll(dir, 0o755); err != nil {
					return err
				}
				if err := os.WriteFile(path, []byte(u.text), 0o644); err != nil {
					return err
				}
			}
		}
		fmt.Fprintf(stdout, "%12s  %s\n", action, path)
	}
	// What a dispatched claude would trip over, said now rather than at the
	// first start.
	for _, w := range cfg.Workspaces {
		if cmd, err := ais.ProfileCommand(cfg.raw, w.Profile); err == nil && len(cmd) > 0 && filepath.Base(cmd[0]) == "claude" {
			if base, err := ais.WorktreeBase(cfg.raw); err == nil && !claudeTrusts(base) {
				note("Claude does not trust %s yet: run claude there once and accept, or dispatched sessions will stop at its trust dialog", base)
			}
			break
		}
	}
	if dry || o.flags["--no-enable"] {
		if !dry {
			note("not enabled; systemctl --user enable --now ais-flow-dispatch.timer (and the ais-flow-plan-* timers) start them")
		}
		return nil
	}
	enable := []string{"enable", "--now", "ais-flow-dispatch.timer"}
	for _, u := range units {
		if strings.HasPrefix(u.name, "ais-flow-plan-") {
			enable = append(enable, u.name)
		}
	}
	steps := [][]string{{"daemon-reload"}, enable}
	if !changed {
		steps = steps[1:]
	}
	for _, step := range steps {
		cmd := exec.Command("systemctl", append([]string{"--user"}, step...)...)
		if out, err := cmd.CombinedOutput(); err != nil {
			return fmt.Errorf("systemctl --user %s: %s", strings.Join(step, " "), strings.TrimSpace(string(out)))
		}
	}
	note("timers are on: systemctl --user list-timers 'ais-flow*'; journalctl --user -u 'ais-flow*'")
	return nil
}
