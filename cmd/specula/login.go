package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/ivanzzeth/specula/internal/clicreds"
)

func runLogin(args []string) error {
	fs := flag.NewFlagSet("login", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	addr := fs.String("addr", "http://127.0.0.1:7733", "Specula control-plane base URL")
	token := fs.String("token", "", "API key (spck_…). If empty, read from stdin")
	skipVerify := fs.Bool("skip-verify", false, "store token without calling GET /api/v1/stats")
	fs.Usage = func() {
		fmt.Fprintf(os.Stderr, `Usage:
  specula login --token spck_… [--addr http://127.0.0.1:7733]

Persist an API key for the Specula CLI (like npm login), written to
~/.config/specula/credentials.json (mode 0600).

Env overrides (also used by specula stats):
  SPECULA_TOKEN
  SPECULA_CONTROL_PLANE / SPECULA_ADDR

Create a key in the WebUI or via POST /api/v1/keys (session JWT), then:
  specula login --token spck_…

Flags:
`)
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return err
	}

	tok := strings.TrimSpace(*token)
	if tok == "" {
		b, err := io.ReadAll(os.Stdin)
		if err != nil {
			return err
		}
		tok = strings.TrimSpace(string(b))
	}
	if tok == "" {
		return fmt.Errorf("token required (pass --token or pipe it on stdin)")
	}

	ctrl := strings.TrimRight(strings.TrimSpace(*addr), "/")
	if !*skipVerify {
		if err := verifyAPIKey(ctrl, tok); err != nil {
			return fmt.Errorf("verify token against %s: %w", ctrl, err)
		}
	}

	if err := clicreds.Save(clicreds.Credentials{
		ControlPlane: ctrl,
		Token:        tok,
	}); err != nil {
		return err
	}
	path, _ := clicreds.Path()
	fmt.Fprintf(os.Stdout, "Logged in. Credentials saved to %s\n", path)
	return nil
}

func runLogout(args []string) error {
	fs := flag.NewFlagSet("logout", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	fs.Usage = func() {
		fmt.Fprintf(os.Stderr, `Usage:
  specula logout

Remove ~/.config/specula/credentials.json.
`)
	}
	if err := fs.Parse(args); err != nil {
		return err
	}
	if err := clicreds.Clear(); err != nil {
		return err
	}
	fmt.Fprintln(os.Stdout, "Logged out.")
	return nil
}

func verifyAPIKey(ctrl, token string) error {
	req, err := http.NewRequest(http.MethodGet, ctrl+"/api/v1/stats", nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("HTTP %d: %s", resp.StatusCode, truncate(string(body), 200))
	}
	var probe map[string]json.RawMessage
	if err := json.Unmarshal(body, &probe); err != nil {
		return fmt.Errorf("unexpected response: %w", err)
	}
	if _, ok := probe["cache"]; !ok {
		return fmt.Errorf("unexpected /api/v1/stats payload (missing cache)")
	}
	if _, ok := probe["traffic"]; !ok {
		return fmt.Errorf("unexpected /api/v1/stats payload (missing traffic)")
	}
	return nil
}
