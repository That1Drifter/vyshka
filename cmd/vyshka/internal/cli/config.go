package cli

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

// configFile is the optional profile file:
//
//	{"default": "home",
//	 "profiles": {"home": {"url": "http://127.0.0.1:8080", "token": "file:/home/me/.vyshka-token"}}}
type configFile struct {
	Default  string                   `json:"default"`
	Profiles map[string]configProfile `json:"profiles"`
}

type configProfile struct {
	URL   string `json:"url"`
	Token string `json:"token"`
}

// target is where a run talks and with what.
type target struct {
	url, token string
}

// configPath is --config, else VYSHKA_CONFIG, else the file under the user's
// configuration directory; "" when there is nowhere to look.
func (e *env) configPath() string {
	if e.g.cfgSet {
		return e.g.config
	}
	if path := e.stdio.Getenv("VYSHKA_CONFIG"); path != "" {
		return path
	}
	dir, err := e.stdio.ConfigDir()
	if err != nil || dir == "" {
		return ""
	}
	return filepath.Join(dir, "vyshka", "config.json")
}

// loadConfig reads the profile file. A missing file is an empty one; a file
// that exists but does not parse is an error, since guessing past it could
// send a token to the wrong hub.
func loadConfig(path string) (configFile, error) {
	var config configFile
	if path == "" {
		return config, nil
	}
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return config, nil
	}
	if err != nil {
		return config, usagef("config file: %v", err)
	}
	if err := json.Unmarshal(data, &config); err != nil {
		return config, usagef("config file %s is not valid: %v", path, err)
	}
	return config, nil
}

// pickProfile chooses the profile: --profile, else VYSHKA_PROFILE, else the
// file's default, else the only profile there is. Naming one the file does
// not have is an error; having none to choose is not.
func (e *env) pickProfile(config configFile, path string) (configProfile, error) {
	name := ""
	switch {
	case e.g.profileSet:
		name = e.g.profile
	case e.stdio.Getenv("VYSHKA_PROFILE") != "":
		name = e.stdio.Getenv("VYSHKA_PROFILE")
	case config.Default != "":
		name = config.Default
	case len(config.Profiles) == 1:
		for only := range config.Profiles {
			return config.Profiles[only], nil
		}
	default:
		return configProfile{}, nil
	}
	if name == "" {
		return configProfile{}, nil
	}
	profile, ok := config.Profiles[name]
	if !ok {
		names := make([]string, 0, len(config.Profiles))
		for known := range config.Profiles {
			names = append(names, known)
		}
		slices.Sort(names)
		where := "the config file"
		if path != "" {
			where = path
		}
		if len(names) == 0 {
			return configProfile{}, usagef("profile %q is not in %s, which has no profiles", name, where)
		}
		return configProfile{}, usagef("profile %q is not in %s; its profiles: %s", name, where, strings.Join(names, ", "))
	}
	return profile, nil
}

// resolveTarget settles the URL and the token, each from the flag, else the
// environment, else the profile, then follows a file: token to its file.
func (e *env) resolveTarget() (target, error) {
	path := e.configPath()
	config, err := loadConfig(path)
	if err != nil {
		return target{}, err
	}
	profile, err := e.pickProfile(config, path)
	if err != nil {
		return target{}, err
	}

	url := profile.URL
	if value := e.stdio.Getenv("VYSHKA_URL"); value != "" {
		url = value
	}
	if e.g.urlSet {
		url = e.g.url
	}
	token := profile.Token
	if value := e.stdio.Getenv("VYSHKA_TOKEN"); value != "" {
		token = value
	}
	if e.g.tokenSet {
		token = e.g.token
	}

	if url == "" {
		return target{}, usagef("no hub URL: pass --url, set VYSHKA_URL, or add a profile to the config file%s", configHint(path))
	}
	if token == "" {
		return target{}, usagef("no token: pass --token, set VYSHKA_TOKEN, or add a profile to the config file%s", configHint(path))
	}
	token, err = resolveSecret(token)
	if err != nil {
		return target{}, usagef("token: %v", err)
	}
	return target{url: url, token: token}, nil
}

func configHint(path string) string {
	if path == "" {
		return ""
	}
	return " (" + path + ")"
}

// resolveSecret follows the file: indirection the hub's own secret flags
// take, so a token can stay out of the process list, the shell history, and
// the config file itself. Errors name the path, never the contents.
func resolveSecret(value string) (string, error) {
	path, isFile := strings.CutPrefix(value, "file:")
	if !isFile {
		return value, nil
	}
	contents, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	secret := strings.TrimSpace(string(contents))
	if secret == "" {
		return "", fmt.Errorf("%s is empty", path)
	}
	return secret, nil
}
