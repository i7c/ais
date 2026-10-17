package main

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/BurntSushi/toml"
)

// Config is ~/.config/tsm/config.toml ($TSM_CONFIG overrides). Everything in
// it is optional; without it tsm manages this host's tmux and nothing else.
//
//	[[remote]]
//	name = "karif"          # how tsm shows it
//	ssh = "karif"           # ssh destination (an alias from ~/.ssh/config)
//	attach = "mosh"         # what "tsm attach karif" connects with: ssh or mosh
//	session = "main"        # the tmux session it attaches to there
//	tsm = "tsm"             # the tsm to run there
type Config struct {
	Remotes []RemoteConf `toml:"remote"`
}

type RemoteConf struct {
	Name    string   `toml:"name"`
	SSH     string   `toml:"ssh"`
	Attach  string   `toml:"attach"`
	Session string   `toml:"session"`
	TSM     string   `toml:"tsm"`
	Command []string `toml:"command"` // instead of ssh: run remote commands with this prefix (tests, containers)
}

// configPath is $TSM_CONFIG, else $XDG_CONFIG_HOME/tsm/config.toml, else
// ~/.config/tsm/config.toml — on every OS. Go's own idea of a config dir is
// ~/Library/Application Support on a Mac, which is not where anybody keeps
// the dotfiles of a terminal tool.
func configPath() string {
	if p := os.Getenv("TSM_CONFIG"); p != "" {
		return p
	}
	if dir := os.Getenv("XDG_CONFIG_HOME"); dir != "" {
		return filepath.Join(dir, "tsm", "config.toml")
	}
	if home == "" {
		return ""
	}
	return filepath.Join(home, ".config", "tsm", "config.toml")
}

func LoadConfig() (Config, error) {
	var c Config
	path := configPath()
	if path == "" {
		return c, nil
	}
	if _, err := os.Stat(path); err != nil {
		return c, nil // no config is a fine config
	}
	if _, err := toml.DecodeFile(path, &c); err != nil {
		return c, fmt.Errorf("%s: %w", tilde(path), err)
	}
	for i := range c.Remotes {
		r := &c.Remotes[i]
		if r.Name == "" {
			r.Name = r.SSH
		}
		if r.Name == "" {
			return c, fmt.Errorf("%s: a [[remote]] needs a name or an ssh destination", tilde(path))
		}
		if r.SSH == "" {
			r.SSH = r.Name
		}
		if r.Attach == "" {
			r.Attach = "ssh"
		}
		if r.Session == "" {
			r.Session = "main"
		}
	}
	return c, nil
}
