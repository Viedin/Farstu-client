package control

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/viedin/farstu-client/api"
)

func TestRegisterRetries(t *testing.T) {
	codes := 0
	polls := []func(w http.ResponseWriter){
		func(w http.ResponseWriter) { http.Error(w, "internal error", 500) }, // transient: retry
		func(w http.ResponseWriter) {
			w.WriteHeader(400)
			json.NewEncoder(w).Encode(api.Error{Error: api.ErrAuthorizationPending})
		},
		func(w http.ResponseWriter) {
			w.WriteHeader(400)
			json.NewEncoder(w).Encode(api.Error{Error: api.ErrInvalidGrant})
		}, // purged: new code
		func(w http.ResponseWriter) {
			json.NewEncoder(w).Encode(api.DeviceTokenResponse{Token: "tok", Subdomain: "calm-otter-1", RelayCert: "PEM"})
		},
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/device/code":
			codes++
			json.NewEncoder(w).Encode(api.DeviceCodeResponse{DeviceCode: "dc", UserCode: "ABCD-EFGH", Interval: 0})
		case "/v1/device/token":
			polls[0](w)
			polls = polls[1:]
		}
	}))
	defer srv.Close()

	path := filepath.Join(t.TempDir(), "device.json")
	shown := 0
	d, err := Register(context.Background(), srv.URL, path, func(api.DeviceCodeResponse) { shown++ })
	if err != nil {
		t.Fatal(err)
	}
	if d.Token != "tok" || codes != 2 || shown != 2 {
		t.Fatalf("token %q after %d codes, %d shown, want tok after 2", d.Token, codes, shown)
	}

	// Saved and reused; no new registration.
	if d, err := Register(context.Background(), "http://unused.invalid", path, nil); err != nil || d.Token != "tok" {
		t.Fatalf("reload: %+v %v", d, err)
	}
	// A corrupt file is refused rather than silently re-registering, and so
	// is one without the relay's cert: frpc would send its token unverified.
	for _, bad := range []string{`{}`, `{"token":"tok"}`} {
		os.WriteFile(path, []byte(bad), 0o600)
		if _, err := Register(context.Background(), "http://unused.invalid", path, nil); err == nil {
			t.Fatalf("device file %s accepted", bad)
		}
	}
}
