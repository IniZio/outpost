package main

import (
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestServeWiresEgressWhenAProxyIsSet(t *testing.T) {
	ca := filepath.Join(t.TempDir(), "ca.pem")
	if err := os.WriteFile(ca, []byte("-----BEGIN CERTIFICATE-----\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	l := startServer(t, map[string]string{
		"OUTPOST_PROXY_URL": "http://ob-proxy:8080", "OUTPOST_PROXY_CA_FILE": ca, "OUTPOST_PROXY_TOKEN": "proxy-token-0123456789",
	})
	res, err := http.Post(l.url+"/internal/egress/grant", "application/json", strings.NewReader(`{}`))
	if err != nil || res.StatusCode != http.StatusUnauthorized {
		t.Fatalf("the grant route asks for the proxy's bearer: %v %v", err, res)
	}
	_ = res.Body.Close()
}
