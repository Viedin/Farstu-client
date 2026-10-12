package control

import (
	"context"
	"encoding/json"
	"errors"
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

// Only a 401 means the device is gone; a server having trouble must not make
// the client forget it.
func TestMeUnknownDevice(t *testing.T) {
	status := http.StatusUnauthorized
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(status) }))
	defer srv.Close()
	if _, err := Me(context.Background(), srv.URL, "tok"); !errors.Is(err, ErrUnknownDevice) {
		t.Fatalf("401: %v", err)
	}
	status = http.StatusBadGateway
	if _, err := Me(context.Background(), srv.URL, "tok"); err == nil || errors.Is(err, ErrUnknownDevice) {
		t.Fatalf("502: %v", err)
	}
	if _, err := Me(context.Background(), "http://127.0.0.1:1", "tok"); err == nil || errors.Is(err, ErrUnknownDevice) {
		t.Fatalf("unreachable: %v", err)
	}
}

// The device token never follows a redirect, nor goes out over plain HTTP.
func TestTokenStaysOnTLS(t *testing.T) {
	leaked := false
	elsewhere := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		leaked = r.Header.Get("Authorization") != ""
	}))
	defer elsewhere.Close()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, elsewhere.URL+r.URL.Path, http.StatusFound)
	}))
	defer srv.Close()
	if _, err := Me(context.Background(), srv.URL, "tok"); err == nil || leaked {
		t.Fatalf("redirect followed: %v, token sent on %v", err, leaked)
	}

	for url, ok := range map[string]bool{
		"https://api.farstu.app:8443": true,
		"http://localhost:8090":       true,
		"http://127.0.0.1:8090":       true,
		"http://[::1]:8090":           true,
		"http://api.farstu.app":       false,
		"http://192.168.1.5:8090":     false,
		"ftp://api.farstu.app":        false,
		"api.farstu.app":              false,
		"https://":                    false,
	} {
		if err := CheckServerURL(url); (err == nil) != ok {
			t.Errorf("%s: %v, want ok %v", url, err, ok)
		}
	}
}
