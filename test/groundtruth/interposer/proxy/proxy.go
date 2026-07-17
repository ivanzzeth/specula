// Package proxy implements a recording HTTP proxy that sits between Specula and
// a real upstream mirror. It exists to answer one question that Specula's own
// counters structurally cannot answer honestly:
//
//	did this request ACTUALLY contact an upstream?
//
// # Why this package exists at all
//
// specula_cache_hits_total, specula_cache_misses_total and
// specula_upstream_latency_seconds are all incremented by the very code paths
// whose behaviour they describe. A single bug therefore satisfies both the
// behaviour and its own measurement, and every test that reads those counters
// agrees with it. That is not hypothetical: serve-stale was dead across five
// handlers while the suite was green, and a caller's ?digest= pin was ignored on
// every cache hit while the cold-path tests passed.
//
// This package is the arbiter. Point Specula's configured upstream base_url at
// an Interposer, point the Interposer at the real CN mirror, and the request log
// it keeps is ground truth about upstream contact — obtained from the wire, not
// from a counter that the code under test also owns.
//
// # The isolation rule
//
// This package imports the Go standard library and NOTHING else. In particular
// it imports no Specula package: not internal/metrics, not internal/cache, not
// internal/stats. If it shared code with the thing it grades, it would be a
// mirror, and a mirror agrees with a lie. Keep it that way — an import of
// anything under internal/ defeats the entire purpose.
//
// It lives in its own package rather than in package main so that the
// arbitration logic is gated by the coverage gate at the normal threshold: an
// arbiter nobody tests just relocates "the thing that measures is the thing
// being measured" into "the thing that judges is unjudged". If it miscounts,
// every verdict built on it is garbage — and confidently, self-consistently so.
package proxy

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"
)

// Failure-injection modes.
//
//	ModeOK   — forward to the real upstream (default).
//	ModeFail — do NOT forward; answer FailStatus immediately. The request is
//	           still recorded: Specula tried, and "tried" is exactly what
//	           serve-stale testing must establish. 503 is classified transient by
//	           internal/upstream, so this is the state that must trigger both
//	           serve-stale and, after enough consecutive failures, the
//	           specula_upstream_blocked gauge.
//	ModeHang — accept and stall past any client timeout, then close.
const (
	ModeOK   = "ok"
	ModeFail = "fail"
	ModeHang = "hang"
)

// HangDuration is how long ModeHang stalls before giving up.
var HangDuration = 90 * time.Second

// Record is one observed upstream contact attempt. One Record is appended for
// every request that reaches the data port, BEFORE it is forwarded, so a request
// that fails or is refused is still counted as contact — that is precisely the
// distinction "did Specula reach out" requires.
type Record struct {
	Seq          int    `json:"seq"`
	TS           string `json:"ts"`
	Method       string `json:"method"`
	Path         string `json:"path"`
	Query        string `json:"query,omitempty"`
	IfNoneMatch  string `json:"if_none_match,omitempty"`
	IfModSince   string `json:"if_modified_since,omitempty"`
	Mode         string `json:"mode"`
	Status       int    `json:"status"`
	RespBytes    int64  `json:"resp_bytes"`
	UpstreamETag string `json:"upstream_etag,omitempty"`
	DurationMS   int64  `json:"duration_ms"`
	Error        string `json:"error,omitempty"`
}

// Interposer is a recording proxy in front of one upstream.
type Interposer struct {
	// Name labels this interposer in logs and /stats.
	Name string
	// Upstream is the real mirror everything is forwarded to.
	Upstream *url.URL
	// Client performs the upstream requests.
	Client *http.Client
	// Delay is an artificial pause before forwarding. It widens the
	// single-flight window deterministically.
	Delay time.Duration
	// FailStatus is returned in ModeFail.
	FailStatus int
	// LogFile, when non-nil, receives every Record as JSONL.
	LogFile io.Writer

	mu      sync.Mutex
	mode    string
	records []Record
	seq     int
}

// New builds an Interposer forwarding to upstream.
func New(name string, upstream *url.URL) *Interposer {
	return &Interposer{
		Name:     name,
		Upstream: upstream,
		mode:     ModeOK,
		// Redirects are followed HERE, deliberately (the default http.Client
		// policy). If the interposer passed a 302 through, Specula's own client
		// would follow it straight to the real mirror — bypassing the arbiter and
		// silently undercounting upstream contact, which is the one number this
		// package exists to produce. Following here keeps every byte on a path we
		// observe.
		//
		// The timeout is deliberately generous: Specula's upstream client has its
		// own hard 30s whole-request timeout, and the interposer must never be the
		// component that decides a fetch failed.
		Client:     &http.Client{Timeout: 120 * time.Second},
		FailStatus: http.StatusServiceUnavailable,
	}
}

// Mode reports the current failure-injection mode.
func (ip *Interposer) Mode() string {
	ip.mu.Lock()
	defer ip.mu.Unlock()
	return ip.mode
}

// SetMode sets the failure-injection mode. It reports false for an unknown mode
// rather than silently leaving the interposer in a state nobody asked for.
func (ip *Interposer) SetMode(m string) bool {
	switch m {
	case ModeOK, ModeFail, ModeHang:
	default:
		return false
	}
	ip.mu.Lock()
	ip.mode = m
	ip.mu.Unlock()
	return true
}

// Records returns a copy of everything observed so far.
func (ip *Interposer) Records() []Record {
	ip.mu.Lock()
	defer ip.mu.Unlock()
	return append([]Record(nil), ip.records...)
}

// Reset drops the record buffer, so a caller can measure each claim as a delta
// from a known zero rather than by arithmetic subtraction.
func (ip *Interposer) Reset() {
	ip.mu.Lock()
	ip.records = nil
	ip.seq = 0
	ip.mu.Unlock()
}

// hopByHop headers must not be forwarded (RFC 7230 §6.1).
var hopByHop = []string{
	"Connection", "Proxy-Connection", "Keep-Alive", "Transfer-Encoding",
	"TE", "Trailer", "Upgrade", "Proxy-Authenticate", "Proxy-Authorization",
}

// IsHopByHop reports whether a header is hop-by-hop and must not be forwarded.
func IsHopByHop(k string) bool {
	for _, h := range hopByHop {
		if strings.EqualFold(k, h) {
			return true
		}
	}
	return false
}

// ServeProxy handles one data-plane request: record it, then either forward it
// to the real upstream or inject a failure.
func (ip *Interposer) ServeProxy(w http.ResponseWriter, r *http.Request) {
	start := time.Now()

	ip.mu.Lock()
	ip.seq++
	rec := Record{
		Seq:         ip.seq,
		TS:          start.UTC().Format(time.RFC3339Nano),
		Method:      r.Method,
		Path:        r.URL.Path,
		Query:       r.URL.RawQuery,
		IfNoneMatch: r.Header.Get("If-None-Match"),
		IfModSince:  r.Header.Get("If-Modified-Since"),
		Mode:        ip.mode,
	}
	mode := ip.mode
	ip.mu.Unlock()

	if ip.Delay > 0 {
		time.Sleep(ip.Delay)
	}

	switch mode {
	case ModeFail:
		rec.Status = ip.FailStatus
		http.Error(w, "interposer: injected upstream failure", ip.FailStatus)
		rec.DurationMS = time.Since(start).Milliseconds()
		ip.commit(rec)
		return
	case ModeHang:
		time.Sleep(HangDuration)
		rec.Error = "hang"
		rec.DurationMS = time.Since(start).Milliseconds()
		ip.commit(rec)
		return
	}

	target := *ip.Upstream
	target.Path = strings.TrimRight(ip.Upstream.Path, "/") + r.URL.Path
	target.RawQuery = r.URL.RawQuery

	req, err := http.NewRequestWithContext(r.Context(), r.Method, target.String(), r.Body)
	if err != nil {
		ip.failRecord(w, rec, start, "interposer: build request: "+err.Error(), err)
		return
	}
	for k, vs := range r.Header {
		if IsHopByHop(k) {
			continue
		}
		for _, v := range vs {
			req.Header.Add(k, v)
		}
	}
	// Host must follow the upstream, not the interposer's own listen address.
	req.Host = ip.Upstream.Host

	resp, err := ip.Client.Do(req)
	if err != nil {
		ip.failRecord(w, rec, start, "interposer: upstream error: "+err.Error(), err)
		return
	}
	defer resp.Body.Close()

	for k, vs := range resp.Header {
		if IsHopByHop(k) {
			continue
		}
		for _, v := range vs {
			w.Header().Add(k, v)
		}
	}
	w.WriteHeader(resp.StatusCode)
	n, _ := io.Copy(w, resp.Body)

	rec.Status = resp.StatusCode
	rec.RespBytes = n
	rec.UpstreamETag = resp.Header.Get("ETag")
	rec.DurationMS = time.Since(start).Milliseconds()
	ip.commit(rec)
}

// failRecord records a failed attempt and reports 502 to the caller. The attempt
// is still a Record: "tried and failed" and "never looked" are different facts.
func (ip *Interposer) failRecord(w http.ResponseWriter, rec Record, start time.Time, msg string, err error) {
	rec.Status = http.StatusBadGateway
	rec.Error = err.Error()
	rec.DurationMS = time.Since(start).Milliseconds()
	ip.commit(rec)
	http.Error(w, msg, http.StatusBadGateway)
}

// commit stores a finished Record and, if configured, appends it to the log.
func (ip *Interposer) commit(rec Record) {
	b, err := json.Marshal(rec)
	ip.mu.Lock()
	ip.records = append(ip.records, rec)
	if ip.LogFile != nil && err == nil {
		_, _ = ip.LogFile.Write(append(b, '\n'))
	}
	ip.mu.Unlock()
}

// Stats is the arbitration summary the gate reads.
type Stats struct {
	Name            string         `json:"name"`
	Mode            string         `json:"mode"`
	FilterPath      string         `json:"filter_path"`
	Total           int            `json:"total"`
	Conditional     int            `json:"conditional"`
	NotModified     int            `json:"not_modified"`
	RespBytes       int64          `json:"resp_bytes"`
	ByPath          map[string]int `json:"by_path"`
	ByStatus        map[string]int `json:"by_status"`
	Upstream        string         `json:"upstream"`
	RecordsAllPaths int            `json:"records_all_paths"`
}

// Stats summarises observed requests, optionally filtered to an exact path.
// Total is what "how many times did Specula touch upstream" means, and is the
// number that arbitrates every claim.
func (ip *Interposer) Stats(filterPath string) Stats {
	ip.mu.Lock()
	defer ip.mu.Unlock()

	s := Stats{
		Name:            ip.Name,
		Mode:            ip.mode,
		FilterPath:      filterPath,
		ByPath:          map[string]int{},
		ByStatus:        map[string]int{},
		Upstream:        ip.Upstream.String(),
		RecordsAllPaths: len(ip.records),
	}
	for _, rec := range ip.records {
		if filterPath != "" && rec.Path != filterPath {
			continue
		}
		s.Total++
		s.ByPath[rec.Path]++
		s.ByStatus[fmt.Sprintf("%d", rec.Status)]++
		s.RespBytes += rec.RespBytes
		if rec.IfNoneMatch != "" || rec.IfModSince != "" {
			s.Conditional++
		}
		if rec.Status == http.StatusNotModified {
			s.NotModified++
		}
	}
	return s
}

// ControlMux is the control-plane API. It is served on its OWN listener so that
// no control path can ever shadow a path the real upstream serves (a mirror may
// legitimately serve /stats).
func (ip *Interposer) ControlMux() http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintln(w, "ok")
	})

	// GET /records — every observed request, in order.
	mux.HandleFunc("/records", func(w http.ResponseWriter, r *http.Request) {
		WriteJSON(w, ip.Records())
	})

	// GET /stats[?path=/some/path] — the count that arbitrates every claim.
	mux.HandleFunc("/stats", func(w http.ResponseWriter, r *http.Request) {
		WriteJSON(w, ip.Stats(r.URL.Query().Get("path")))
	})

	// POST /reset — drop the record buffer.
	mux.HandleFunc("/reset", func(w http.ResponseWriter, r *http.Request) {
		ip.Reset()
		fmt.Fprintln(w, "reset")
	})

	// POST /mode?m=ok|fail|hang — failure injection.
	mux.HandleFunc("/mode", func(w http.ResponseWriter, r *http.Request) {
		m := r.URL.Query().Get("m")
		if !ip.SetMode(m) {
			http.Error(w, "bad mode: "+m, http.StatusBadRequest)
			return
		}
		fmt.Fprintln(w, m)
	})

	return mux
}

// WriteJSON writes v as indented JSON.
func WriteJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	_ = enc.Encode(v)
}

// WritePorts records the chosen ports so a shell caller can discover them
// without parsing log output.
func WritePorts(path, name, data, control, upstream string, pid int) error {
	b, err := json.MarshalIndent(map[string]any{
		"name":     name,
		"data":     data,
		"control":  control,
		"upstream": upstream,
		"pid":      pid,
	}, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, b, 0o644)
}
