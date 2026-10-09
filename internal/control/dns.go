package control

import (
	"context"
	"fmt"
	"net/http"

	"github.com/libdns/libdns"

	"github.com/viedin/farstu-client/api"
)

// DNSProvider is a libdns provider for certmagic's DNS-01 solver. It writes
// TXT records through Farstu Server, which only allows this device's own
// _acme-challenge name.
type DNSProvider struct {
	ServerURL string
	Token     string
}

func (p *DNSProvider) AppendRecords(ctx context.Context, zone string, recs []libdns.Record) ([]libdns.Record, error) {
	return p.send(ctx, http.MethodPut, zone, recs)
}

func (p *DNSProvider) DeleteRecords(ctx context.Context, zone string, recs []libdns.Record) ([]libdns.Record, error) {
	return p.send(ctx, http.MethodDelete, zone, recs)
}

func (p *DNSProvider) send(ctx context.Context, method, zone string, recs []libdns.Record) ([]libdns.Record, error) {
	var done []libdns.Record
	for _, rec := range recs {
		rr := rec.RR()
		if rr.Type != "TXT" {
			return done, fmt.Errorf("only TXT records are supported, got %s", rr.Type)
		}
		txt := api.TXTRecord{Name: libdns.AbsoluteName(rr.Name, zone), Value: rr.Data}
		status, body, err := do(ctx, method, p.ServerURL+"/v1/dns/txt", p.Token, txt)
		if err == nil && status != http.StatusNoContent {
			err = fmt.Errorf("HTTP %d: %s", status, body)
		}
		if err != nil {
			return done, fmt.Errorf("%s TXT %s: %w", method, txt.Name, err)
		}
		done = append(done, rec)
	}
	return done, nil
}
