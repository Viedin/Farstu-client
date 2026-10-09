package recipes

import (
	"testing"
	"testing/fstest"

	"github.com/viedin/farstu-client/recipes"
)

func TestRecipes(t *testing.T) {
	rs, err := Load(recipes.FS)
	if err != nil || len(rs) == 0 {
		t.Fatalf("shipped recipes: %d, %v", len(rs), err)
	}
	for image, want := range map[string]string{
		"ghcr.io/immich-app/immich-server:v1.120.0":          "immich",
		"ghcr.io/immich-app/immich-server@sha256:ab":         "immich",
		"nextcloud:apache":                                   "nextcloud",
		"docker.io/library/nextcloud":                        "nextcloud",
		"neosmemo/memos:0.28.0":                              "memos",
		"docmost/docmost:0.90.0":                             "docmost",
		"gitea/gitea:1.26.2-rootless":                        "gitea",
		"docker.gitea.com/gitea:1.26":                        "gitea",
		"adguard/adguardhome:v0.107.79":                      "adguard",
		"ghcr.io/diygod/rsshub:latest":                       "rsshub",
		"excalidraw/excalidraw":                              "excalidraw",
		"localhost:5000/nextcloud:29":                        "",
		"ghcr.io/immich-app/immich-machine-learning:release": "",
		"sha256:0123abcd":                                    "",
	} {
		r, _ := Find(rs, image)
		if r.App != want {
			t.Errorf("%s: %q, want %q", image, r.App, want)
		}
	}
}

// A recipe that can't say whether its app is safe to open is refused.
func TestLoadRefuses(t *testing.T) {
	for name, y := range map[string]string{
		"no setup check":         `{app: a, match: {images: [a]}, upstream_port: 80}`,
		"setup check and open":   `{app: a, match: {images: [a]}, upstream_port: 80, no_login: true, setup_done: {path: /x, status: 401}}`,
		"200 without a field":    `{app: a, match: {images: [a]}, upstream_port: 80, setup_done: {path: /x}}`,
		"login without a method": `{app: a, match: {images: [a]}, upstream_port: 80, no_login: true, login: [{path: /x}]}`,
		"old single image":       `{app: a, match: {image: a}, upstream_port: 80, no_login: true}`,
	} {
		if _, err := Load(fstest.MapFS{"a.yaml": {Data: []byte(y)}}); err == nil {
			t.Errorf("%s: loaded", name)
		}
	}
}
