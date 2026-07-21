package bench

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestRunColdWarm(t *testing.T) {
	var hits int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		body := strings.Repeat("x", 1024*100) // 100 KiB
		w.Header().Set("Content-Length", fmt.Sprintf("%d", len(body)))
		_, _ = io.WriteString(w, body)
	}))
	t.Cleanup(srv.Close)

	// Override cases by hitting a path that our selectCases won't use —
	// exercise runCase + FormatTable via a custom HTTPClient against a
	// minimal fake using only the go case path if we mount it.
	mux := http.NewServeMux()
	mux.HandleFunc("/go/", func(w http.ResponseWriter, r *http.Request) {
		hits++
		body := strings.Repeat("g", 50*1024)
		_, _ = io.WriteString(w, body)
	})
	mux.HandleFunc("/token", func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"token":"test-token"}`)
	})
	mux.HandleFunc("/v2/", func(w http.ResponseWriter, r *http.Request) {
		hits++
		_, _ = io.WriteString(w, `{"schemaVersion":2}`)
	})
	fake := httptest.NewServer(mux)
	t.Cleanup(fake.Close)

	rep, err := Run(context.Background(), Options{
		Addr:       fake.URL,
		Protocols:  []string{"go", "oci"},
		WarmRounds: 1,
		Timeout:    5 * time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.Results) != 4 { // go cold/warm + oci cold/warm
		t.Fatalf("rows=%d %+v", len(rep.Results), rep.Results)
	}
	for _, r := range rep.Results {
		if r.Err != "" {
			t.Fatalf("row err: %+v", r)
		}
		if r.Bytes <= 0 || r.Seconds < 0 {
			t.Fatalf("bad timing: %+v", r)
		}
		if r.Pass != "cold" && r.Pass != "warm" {
			t.Fatalf("pass=%q", r.Pass)
		}
	}
	table := FormatTable(rep)
	if !strings.Contains(table, "PROTO") || !strings.Contains(table, "MB/s") {
		t.Fatalf("table:\n%s", table)
	}
	_ = hits
	_ = srv
}

func TestFormatBytes(t *testing.T) {
	if got := formatBytes(1536); !strings.Contains(got, "KiB") {
		t.Fatalf("got %q", got)
	}
	if got := formatBytes(3 << 20); !strings.Contains(got, "MiB") {
		t.Fatalf("got %q", got)
	}
}

func TestRunColdOnly(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/npm/", func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, "npm-body")
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	rep, err := Run(context.Background(), Options{
		Addr:       srv.URL,
		Protocols:  []string{"npm"},
		WarmRounds: 0,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.Results) != 1 || rep.Results[0].Pass != "cold" {
		t.Fatalf("%+v", rep.Results)
	}
}
