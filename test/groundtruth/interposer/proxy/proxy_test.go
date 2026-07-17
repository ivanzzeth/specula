package proxy

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
)

// The interposer is the ARBITER: groundtruth-gate.sh resolves every
// disagreement between Specula and reality by asking this program what it saw
// on the wire. So an untested interposer just relocates the original problem —
// "the thing that measures is the thing being measured" becomes "the thing that
// judges is unjudged". If it miscounts, every verdict it hands down is garbage,
// and it would be garbage in the confident, self-consistent way that is exactly
// what this whole exercise exists to defeat.
//
// These tests are hermetic (httptest upstream, no network) and fast, so they run
// in the default -short unit loop rather than only in the slow network gate.

// newTestInterposer builds an interposer pointed at a test upstream, via the
// real constructor so the tests exercise production construction rather than a
// hand-assembled struct that could drift away from it.
func newTestInterposer(t *testing.T, upstreamURL string) *Interposer {
	t.Helper()
	u, err := url.Parse(upstreamURL)
	if err != nil {
		t.Fatalf("parse upstream: %v", err)
	}
	ip := New("test", u)
	ip.Client = &http.Client{Timeout: 10 * time.Second}
	return ip
}

// TestForwardsAndRecords is the core property: a request through the data port
// reaches the real upstream, the response comes back intact, and exactly one
// record is kept. "Exactly one" is load-bearing — the stampede claim is decided
// by comparing this count against 1.
func TestForwardsAndRecords(t *testing.T) {
	var upstreamHits int32
	var mu sync.Mutex
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		upstreamHits++
		mu.Unlock()
		w.Header().Set("ETag", `"abc"`)
		_, _ = io.WriteString(w, "hello-upstream")
	}))
	defer up.Close()

	ip := newTestInterposer(t, up.URL)
	proxy := httptest.NewServer(http.HandlerFunc(ip.ServeProxy))
	defer proxy.Close()

	resp, err := http.Get(proxy.URL + "/rsc.io/quote/@v/list")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)

	if string(body) != "hello-upstream" {
		t.Errorf("body = %q, want %q", body, "hello-upstream")
	}
	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want 200", resp.StatusCode)
	}
	if upstreamHits != 1 {
		t.Errorf("upstream saw %d requests, want 1", upstreamHits)
	}
	if len(ip.Records()) != 1 {
		t.Fatalf("recorded %d requests, want 1", len(ip.Records()))
	}
	rec := ip.Records()[0]
	if rec.Path != "/rsc.io/quote/@v/list" {
		t.Errorf("recorded path = %q", rec.Path)
	}
	if rec.Status != http.StatusOK {
		t.Errorf("recorded status = %d, want 200", rec.Status)
	}
	if rec.RespBytes != int64(len("hello-upstream")) {
		t.Errorf("recorded bytes = %d, want %d", rec.RespBytes, len("hello-upstream"))
	}
	if rec.UpstreamETag != `"abc"` {
		t.Errorf("recorded etag = %q", rec.UpstreamETag)
	}
}

// TestUpstreamBasePathIsPreserved guards the flat-repo case. A base URL with a
// path component (mirror.azure.cn/kubernetes/charts) must have that prefix
// joined to the incoming path, not dropped. Getting this wrong once cost an
// agent a cycle "finding a bug" that was really a mangled URL.
func TestUpstreamBasePathIsPreserved(t *testing.T) {
	var gotPath string
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
	}))
	defer up.Close()

	ip := newTestInterposer(t, up.URL+"/kubernetes/charts")
	proxy := httptest.NewServer(http.HandlerFunc(ip.ServeProxy))
	defer proxy.Close()

	resp, err := http.Get(proxy.URL + "/index.yaml")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	resp.Body.Close()

	if want := "/kubernetes/charts/index.yaml"; gotPath != want {
		t.Errorf("upstream got path %q, want %q", gotPath, want)
	}
}

// TestConditionalRequestIsRecorded covers the evidence behind the documented
// "hit != no upstream contact" caveat: the interposer must faithfully relay
// If-None-Match and record both the conditional-ness and a resulting 304.
// Without this, a 304 revalidation and a true zero-contact hit look identical.
func TestConditionalRequestIsRecorded(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("If-None-Match") == `"v1"` {
			w.WriteHeader(http.StatusNotModified)
			return
		}
		w.Header().Set("ETag", `"v1"`)
		_, _ = io.WriteString(w, "payload")
	}))
	defer up.Close()

	ip := newTestInterposer(t, up.URL)
	proxy := httptest.NewServer(http.HandlerFunc(ip.ServeProxy))
	defer proxy.Close()

	req, _ := http.NewRequest(http.MethodGet, proxy.URL+"/pkg", nil)
	req.Header.Set("If-None-Match", `"v1"`)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	resp.Body.Close()

	if resp.StatusCode != http.StatusNotModified {
		t.Fatalf("status = %d, want 304", resp.StatusCode)
	}
	if len(ip.Records()) != 1 {
		t.Fatalf("recorded %d, want 1", len(ip.Records()))
	}
	if ip.Records()[0].IfNoneMatch != `"v1"` {
		t.Errorf("If-None-Match not recorded: %q", ip.Records()[0].IfNoneMatch)
	}
	if ip.Records()[0].Status != http.StatusNotModified {
		t.Errorf("status not recorded as 304: %d", ip.Records()[0].Status)
	}
}

// TestModeFailRecordsTheAttempt is the serve-stale precondition. In mode=fail
// the upstream must NOT be contacted, the client must get the failure status,
// and — critically — the attempt must STILL be recorded. "Specula tried and the
// upstream refused" is a different fact from "Specula never looked", and only
// the record distinguishes them.
func TestModeFailRecordsTheAttempt(t *testing.T) {
	var upstreamHits int
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamHits++
	}))
	defer up.Close()

	ip := newTestInterposer(t, up.URL)
	ip.SetMode(ModeFail)
	proxy := httptest.NewServer(http.HandlerFunc(ip.ServeProxy))
	defer proxy.Close()

	resp, err := http.Get(proxy.URL + "/simple/six/")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	resp.Body.Close()

	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503", resp.StatusCode)
	}
	if upstreamHits != 0 {
		t.Errorf("upstream was contacted %d times in mode=fail, want 0", upstreamHits)
	}
	if len(ip.Records()) != 1 {
		t.Fatalf("recorded %d attempts, want 1 — a failed attempt is still contact", len(ip.Records()))
	}
	if ip.Records()[0].Mode != ModeFail {
		t.Errorf("record mode = %q, want %q", ip.Records()[0].Mode, ModeFail)
	}
}

// TestConcurrentRequestsAreAllCounted is the property the stampede claim rests
// on: if the interposer dropped or coalesced concurrent records, it would
// FABRICATE evidence of single-flight that does not exist — reporting the very
// bug it is supposed to detect as fixed.
func TestConcurrentRequestsAreAllCounted(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(10 * time.Millisecond)
		_, _ = io.WriteString(w, "x")
	}))
	defer up.Close()

	ip := newTestInterposer(t, up.URL)
	proxy := httptest.NewServer(http.HandlerFunc(ip.ServeProxy))
	defer proxy.Close()

	const n = 10
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			resp, err := http.Get(proxy.URL + "/same/artifact")
			if err == nil {
				resp.Body.Close()
			}
		}()
	}
	wg.Wait()

	if len(ip.Records()) != n {
		t.Errorf("recorded %d concurrent requests, want %d", len(ip.Records()), n)
	}
}

// TestStatsAndReset covers the control API the gate actually reads: filtered
// counts must be exact, and reset must zero the buffer so each claim measures a
// delta from a known zero rather than an arithmetic subtraction.
func TestStatsAndReset(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, "ok")
	}))
	defer up.Close()

	ip := newTestInterposer(t, up.URL)
	proxy := httptest.NewServer(http.HandlerFunc(ip.ServeProxy))
	defer proxy.Close()
	ctrl := httptest.NewServer(ip.ControlMux())
	defer ctrl.Close()

	for _, p := range []string{"/a", "/a", "/b"} {
		resp, err := http.Get(proxy.URL + p)
		if err != nil {
			t.Fatalf("get %s: %v", p, err)
		}
		resp.Body.Close()
	}

	getStats := func(q string) map[string]any {
		resp, err := http.Get(ctrl.URL + "/stats" + q)
		if err != nil {
			t.Fatalf("stats: %v", err)
		}
		defer resp.Body.Close()
		var m map[string]any
		if err := json.NewDecoder(resp.Body).Decode(&m); err != nil {
			t.Fatalf("decode stats: %v", err)
		}
		return m
	}

	if got := getStats("")["total"].(float64); got != 3 {
		t.Errorf("total = %v, want 3", got)
	}
	if got := getStats("?path=/a")["total"].(float64); got != 2 {
		t.Errorf("total for /a = %v, want 2", got)
	}
	if got := getStats("?path=/b")["total"].(float64); got != 1 {
		t.Errorf("total for /b = %v, want 1", got)
	}
	if got := getStats("?path=/nope")["total"].(float64); got != 0 {
		t.Errorf("total for /nope = %v, want 0", got)
	}

	resp, err := http.Post(ctrl.URL+"/reset", "", nil)
	if err != nil {
		t.Fatalf("reset: %v", err)
	}
	resp.Body.Close()

	if got := getStats("")["total"].(float64); got != 0 {
		t.Errorf("total after reset = %v, want 0", got)
	}
}

// TestModeEndpoint checks the failure-injection switch, including that a bogus
// mode is rejected rather than silently leaving the interposer in a state the
// gate did not ask for.
func TestModeEndpoint(t *testing.T) {
	ip := newTestInterposer(t, "http://127.0.0.1:1")
	ctrl := httptest.NewServer(ip.ControlMux())
	defer ctrl.Close()

	for _, m := range []string{ModeFail, ModeHang, ModeOK} {
		resp, err := http.Post(ctrl.URL+"/mode?m="+m, "", nil)
		if err != nil {
			t.Fatalf("mode %s: %v", m, err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("mode %s: status %d", m, resp.StatusCode)
		}
		if ip.Mode() != m {
			t.Errorf("mode = %q, want %q", ip.Mode(), m)
		}
	}

	resp, err := http.Post(ctrl.URL+"/mode?m=bogus", "", nil)
	if err != nil {
		t.Fatalf("bogus mode: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("bogus mode status = %d, want 400", resp.StatusCode)
	}
	if ip.Mode() != ModeOK {
		t.Errorf("bogus mode changed state to %q", ip.Mode())
	}
}

// TestUpstreamErrorIsRecorded: when the upstream is unreachable the interposer
// must report a gateway error AND record the attempt with the error text.
func TestUpstreamErrorIsRecorded(t *testing.T) {
	// Port 1 is reserved and refuses connections.
	ip := newTestInterposer(t, "http://127.0.0.1:1")
	proxy := httptest.NewServer(http.HandlerFunc(ip.ServeProxy))
	defer proxy.Close()

	resp, err := http.Get(proxy.URL + "/x")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	resp.Body.Close()

	if resp.StatusCode != http.StatusBadGateway {
		t.Errorf("status = %d, want 502", resp.StatusCode)
	}
	if len(ip.Records()) != 1 {
		t.Fatalf("recorded %d, want 1", len(ip.Records()))
	}
	if ip.Records()[0].Error == "" {
		t.Error("upstream error was not recorded")
	}
}

// TestHopByHopHeadersAreStripped: forwarding Connection/Keep-Alive upstream is
// a protocol violation (RFC 7230 §6.1).
func TestHopByHopHeadersAreStripped(t *testing.T) {
	for _, h := range hopByHop {
		if !IsHopByHop(strings.ToLower(h)) {
			t.Errorf("IsHopByHop(%q) = false, want true (must be case-insensitive)", h)
		}
	}
	if IsHopByHop("If-None-Match") {
		t.Error("If-None-Match must NOT be treated as hop-by-hop: the 304 evidence depends on it")
	}
}

// TestLogFileReceivesJSONL: the gate keeps the JSONL log as the durable audit
// trail behind every verdict, so a human can re-read what the arbiter saw
// without re-running the gate.
func TestLogFileReceivesJSONL(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, "z")
	}))
	defer up.Close()

	var buf strings.Builder
	ip := newTestInterposer(t, up.URL)
	ip.LogFile = &buf
	proxy := httptest.NewServer(http.HandlerFunc(ip.ServeProxy))
	defer proxy.Close()

	resp, err := http.Get(proxy.URL + "/logged")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	resp.Body.Close()

	line := strings.TrimSpace(buf.String())
	if line == "" {
		t.Fatal("nothing written to the log")
	}
	var rec Record
	if err := json.Unmarshal([]byte(line), &rec); err != nil {
		t.Fatalf("log line is not valid JSON: %v (%q)", err, line)
	}
	if rec.Path != "/logged" {
		t.Errorf("logged path = %q, want /logged", rec.Path)
	}
}

// TestWritePorts: the shell gate discovers the kernel-assigned ports by reading
// this file. If it were malformed the gate could not address the interposer at
// all — and a gate that cannot reach its arbiter must fail loudly, not guess.
func TestWritePorts(t *testing.T) {
	path := t.TempDir() + "/ports.json"
	if err := WritePorts(path, "gomod", "127.0.0.1:1111", "127.0.0.1:2222",
		"https://goproxy.cn", 4242); err != nil {
		t.Fatalf("WritePorts: %v", err)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("ports file is not valid JSON: %v", err)
	}
	for k, want := range map[string]any{
		"name": "gomod", "data": "127.0.0.1:1111",
		"control": "127.0.0.1:2222", "upstream": "https://goproxy.cn",
	} {
		if m[k] != want {
			t.Errorf("ports[%q] = %v, want %v", k, m[k], want)
		}
	}
	if m["pid"].(float64) != 4242 {
		t.Errorf("ports[pid] = %v, want 4242", m["pid"])
	}
}
