// Package recipes parses the per-app recipes in recipes/.
package recipes

import (
	"errors"
	"fmt"
	"io/fs"
	"path"
	"regexp"
	"slices"
	"strings"

	"go.yaml.in/yaml/v3"
)

type Recipe struct {
	App   string `yaml:"app"` // DNS label: <app>.<user>.<domain>
	Match struct {
		Images []string `yaml:"images"` // repositories, without tag
	} `yaml:"match"`
	UpstreamPort int `yaml:"upstream_port"` // inside the container
	// SetupDone tells a fresh install, whose first visitor becomes its
	// admin, from one that has an admin. Until it is set up the app can't
	// be turned on.
	SetupDone SetupDone `yaml:"setup_done"`
	// NoLogin marks an app without accounts that keeps nothing on the
	// server (e.g. Excalidraw): nothing to set up, anyone may use it.
	NoLogin bool    `yaml:"no_login"`
	Login   []Login `yaml:"login"` // optional
}

// SetupDone: Method (default GET) Path answers Status (default 200) once
// the app is set up. With 200 it must also say so in its answer: Field set
// (true, or a non-empty list or object), none of Unset set (missing, false or
// empty), and the text Contains. Field and Unset are dotted JSON paths
// ("data.account", "setup.0.status"; "." is the whole answer).
type SetupDone struct {
	Method   string   `yaml:"method"`
	Path     string   `yaml:"path"`
	Status   int      `yaml:"status"`
	Field    string   `yaml:"field"`
	Unset    []string `yaml:"unset"`
	Contains string   `yaml:"contains"`
	Message  string   `yaml:"message"` // why it can't be turned on yet; default: no admin account
}

// Login is where the app takes passwords; attempts there are rate limited
// per visitor.
type Login struct {
	Path   string `yaml:"path"`   // e.g. /api/auth/login
	Method string `yaml:"method"` // e.g. POST
}

var label = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]*[a-z0-9])?$`)

// Load parses every *.yaml in fsys.
func Load(fsys fs.FS) ([]Recipe, error) {
	paths, err := fs.Glob(fsys, "*.yaml")
	if err != nil {
		return nil, err
	}
	var rs []Recipe
	for _, p := range paths {
		var r Recipe
		b, err := fs.ReadFile(fsys, p)
		if err == nil {
			err = yaml.Unmarshal(b, &r)
		}
		s := r.SetupDone
		if err == nil && (!label.MatchString(r.App) || len(r.Match.Images) == 0 || slices.Contains(r.Match.Images, "") || r.UpstreamPort <= 0) {
			err = errors.New("needs app (a DNS label), match.images and upstream_port")
		}
		if err == nil && r.NoLogin == (s.Path != "") {
			err = errors.New("needs either setup_done.path or no_login")
		}
		if err == nil && s.Path != "" && (s.Status == 0 || s.Status == 200) && s.Field == "" && len(s.Unset) == 0 && s.Contains == "" {
			err = errors.New("setup_done needs a field, unset or contains, or a status other than 200")
		}
		for _, l := range r.Login {
			if err == nil && (l.Path == "" || l.Method == "") {
				err = errors.New("each login needs path and method")
			}
			// The proxy matches cleaned request paths ("/login/" as "/login").
			if err == nil && path.Clean("/"+l.Path) != l.Path {
				err = fmt.Errorf("login path %s never matches; write it as %s", l.Path, path.Clean("/"+l.Path))
			}
		}
		if err != nil {
			return nil, fmt.Errorf("recipe %s: %w", p, err)
		}
		rs = append(rs, r)
	}
	return rs, nil
}

// Find returns the recipe whose image matches a container's image, e.g.
// "ghcr.io/immich-app/immich-server:v1.120.0" or "nextcloud:apache".
func Find(rs []Recipe, image string) (Recipe, bool) {
	for _, r := range rs {
		for _, img := range r.Match.Images {
			if Repo(img) == Repo(image) {
				return r, true
			}
		}
	}
	return Recipe{}, false
}

// Repo strips the tag or digest and Docker Hub's implied prefixes:
// "docker.io/library/nextcloud:apache" is "nextcloud".
// ponytail: a container whose tag was re-pulled shows a bare image ID and
// is not found; look up the ID's RepoTags if that bites.
func Repo(image string) string {
	image, _, _ = strings.Cut(image, "@")
	if i := strings.LastIndex(image, ":"); i > strings.LastIndex(image, "/") {
		image = image[:i]
	}
	image = strings.TrimPrefix(image, "docker.io/")
	return strings.TrimPrefix(image, "library/")
}
