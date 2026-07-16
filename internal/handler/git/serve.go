// Package git — git http-backend CGI bridge (ported from ai-sandbox gitproxy/serve.go).
//
// serveGitHTTPBackend invokes `git http-backend` as a subprocess to serve Smart
// HTTP requests from a bare mirror on disk. The response is buffered before
// headers are written so the CGI status line can be respected.
//
// NOTE: Buffering the packfile response is acceptable for typical repos but
// becomes memory-intensive for multi-GB packs. A future iteration can use a
// streaming reader that pipes git http-backend stdout directly to the response
// body after the header section is parsed.
package git

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"strings"
)

// serveGitHTTPBackend runs `git http-backend` as a CGI subprocess and forwards
// its response to w.
//
//   - projectRoot is the root directory passed as GIT_PROJECT_ROOT (bare repos
//     live directly under this directory; e.g., projectRoot/<host>/<repo>.git).
//   - pathInfo is the PATH_INFO CGI variable (e.g., "/github.com/o/r.git/info/refs").
func serveGitHTTPBackend(w http.ResponseWriter, r *http.Request, projectRoot, pathInfo string) error {
	cmd := exec.Command("git", "http-backend")
	var stdout, stderr bytes.Buffer
	cmd.Env = append(os.Environ(),
		"GIT_HTTP_EXPORT_ALL=1",
		"GIT_PROJECT_ROOT="+projectRoot,
		"PATH_INFO="+pathInfo,
		"QUERY_STRING="+r.URL.RawQuery,
		"REQUEST_METHOD="+r.Method,
		"SERVER_PROTOCOL=HTTP/1.1",
		"CONTENT_TYPE="+r.Header.Get("Content-Type"),
		"CONTENT_LENGTH="+r.Header.Get("Content-Length"),
	)
	// Propagate Git-Protocol header so git-http-backend activates protocol v2
	// when the client requests it.
	//
	// gitprotocol-v2 §HTTP Transport: "If the Git-Protocol header is set, this
	// is passed through to git-upload-pack as GIT_PROTOCOL."
	// git-http-backend reads GIT_PROTOCOL (not the CGI-conventional
	// HTTP_GIT_PROTOCOL) to select the wire protocol version.
	if gitProto := r.Header.Get("Git-Protocol"); gitProto != "" {
		cmd.Env = append(cmd.Env, "GIT_PROTOCOL="+gitProto)
	}
	cmd.Stdin = r.Body
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = err.Error()
		}
		return fmt.Errorf("git http-backend: %s", msg)
	}
	return writeCGIResponse(w, stdout.Bytes())
}

// writeCGIResponse parses the CGI response (status line + headers + body) and
// writes them to the http.ResponseWriter.
func writeCGIResponse(w http.ResponseWriter, raw []byte) error {
	// Locate the header/body separator. CGI may use \r\n\r\n or \n\n.
	headerEnd := bytes.Index(raw, []byte("\r\n\r\n"))
	sepLen := 4
	if headerEnd < 0 {
		headerEnd = bytes.Index(raw, []byte("\n\n"))
		sepLen = 2
	}
	if headerEnd < 0 {
		// No separator at all — treat everything as body with 200 OK.
		w.WriteHeader(http.StatusOK)
		_, err := w.Write(raw)
		return err
	}

	headerBlock := string(raw[:headerEnd])
	body := raw[headerEnd+sepLen:]

	status := http.StatusOK
	headers := make(http.Header)
	for _, line := range strings.Split(headerBlock, "\n") {
		line = strings.TrimRight(line, "\r")
		if line == "" {
			continue
		}
		// CGI "Status:" pseudo-header overrides the HTTP status code.
		if strings.HasPrefix(strings.ToLower(line), "status:") {
			fields := strings.Fields(line)
			if len(fields) >= 2 {
				var code int
				if _, err := fmt.Sscanf(fields[1], "%d", &code); err == nil {
					status = code
				}
			}
			continue
		}
		if i := strings.IndexByte(line, ':'); i > 0 {
			k := strings.TrimSpace(line[:i])
			v := strings.TrimSpace(line[i+1:])
			headers.Add(k, v)
		}
	}

	for k, vs := range headers {
		for _, v := range vs {
			w.Header().Add(k, v)
		}
	}
	w.WriteHeader(status)
	if len(body) > 0 {
		_, err := io.Copy(w, bytes.NewReader(body))
		return err
	}
	return nil
}
