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
		"lscr.io/linuxserver/jellyfin:10.11.11":              "jellyfin",
		"ghcr.io/advplyr/audiobookshelf:2.37.1":              "audiobookshelf",
		"vaultwarden/server:1.37.4":                          "vaultwarden",
		"codeberg.org/forgejo/forgejo:16.0.5":                "forgejo",
		"ghcr.io/seerr-team/seerr:v3.5.0":                    "seerr",
		"fallenbagel/jellyseerr:2.7.3":                       "jellyseerr",
		"ghcr.io/linuxserver/overseerr:1.35.0":               "overseerr",
		"actualbudget/actual-server:26.10.0":                 "actual",
		"n8nio/n8n:2.43.3":                                   "n8n",
		"ghcr.io/open-webui/open-webui:v0.11.4":              "open-webui",
		"ghcr.io/mealie-recipes/mealie:v3.28.0":              "mealie",
		"ghcr.io/paperless-ngx/paperless-ngx:3.3.0":          "paperless",
		"ghcr.io/triliumnext/trilium:v0.106.0":               "trilium",
		"ghost:5.130.6":                                      "ghost",
		"jvmilazz0/kavita:0.9.1":                             "kavita",
		"ghcr.io/gotson/komga:1.28.1":                        "komga",
		"deluan/navidrome:0.64.2":                            "navidrome",
		"corentinth/it-tools:latest":                         "it-tools",
		"ghcr.io/gchq/cyberchef:11.5.0":                      "cyberchef",
		"donetick/donetick:v0.1.60":                          "donetick",
		"ghcr.io/codewec/dashlit:v1.0.0":                     "dashlit",
		"ghcr.io/karakeep-app/karakeep:release":              "karakeep",
		"ghcr.io/hoarder-app/hoarder:0.22.0":                 "karakeep",
		"ghcr.io/linkwarden/linkwarden:v2.13.0":              "linkwarden",
		"ghcr.io/teableio/teable:latest":                     "teable",
		"ghcr.io/teableio/teable-community:latest":           "teable",
		"wechatopenai/weknora-ui:v0.5.0":                     "weknora",
		"wechatopenai/weknora-app:v0.5.0":                    "",
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
		"login with a slash":     `{app: a, match: {images: [a]}, upstream_port: 80, no_login: true, login: [{method: POST, path: /x/}]}`,
		"relative login":         `{app: a, match: {images: [a]}, upstream_port: 80, no_login: true, login: [{method: POST, path: x}]}`,
	} {
		if _, err := Load(fstest.MapFS{"a.yaml": {Data: []byte(y)}}); err == nil {
			t.Errorf("%s: loaded", name)
		}
	}
}
