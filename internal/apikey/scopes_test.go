package apikey_test

import (
	"testing"

	"github.com/ivanzzeth/specula/internal/apikey"
)

func TestNormalizeScopes_EmptyDefaults(t *testing.T) {
	got := apikey.NormalizeScopes(nil)
	if len(got) != 2 || got[0] != "pull" || got[1] != "push" {
		t.Fatalf("NormalizeScopes(nil) = %v; want [pull push]", got)
	}
}

func TestNormalizeScopes_DedupeAndDropUnknown(t *testing.T) {
	got := apikey.NormalizeScopes([]string{"PUSH", "pull", "pull", "admin"})
	if len(got) != 2 || got[0] != "pull" || got[1] != "push" {
		t.Fatalf("got %v; want [pull push]", got)
	}
}

func TestNormalizeScopes_PullOnly(t *testing.T) {
	got := apikey.NormalizeScopes([]string{"pull"})
	if len(got) != 1 || got[0] != "pull" {
		t.Fatalf("got %v; want [pull]", got)
	}
}

func TestEncodeDecodeScopes_RoundTrip(t *testing.T) {
	enc := apikey.EncodeScopes([]string{"pull"})
	got := apikey.DecodeScopes(enc)
	if len(got) != 1 || got[0] != "pull" {
		t.Fatalf("DecodeScopes(%q) = %v; want [pull]", enc, got)
	}
	// Empty column → default pull+push (pre-scope keys).
	got = apikey.DecodeScopes("")
	if len(got) != 2 {
		t.Fatalf("DecodeScopes(\"\") = %v; want default pull+push", got)
	}
}

func TestAllowsAction(t *testing.T) {
	pullOnly := []string{"pull"}
	full := []string{"pull", "push"}
	cases := []struct {
		scopes []string
		action string
		want   bool
	}{
		{pullOnly, "pull", true},
		{pullOnly, "push", false},
		{pullOnly, "delete", false},
		{full, "pull", true},
		{full, "push", true},
		{full, "delete", true},
		{full, "unknown", false},
		{nil, "pull", true}, // empty → DefaultScopes
		{nil, "push", true},
	}
	for _, tc := range cases {
		if got := apikey.AllowsAction(tc.scopes, tc.action); got != tc.want {
			t.Errorf("AllowsAction(%v, %q) = %v; want %v", tc.scopes, tc.action, got, tc.want)
		}
	}
}

func TestCreateWithScopes_MemAndSQL(t *testing.T) {
	for _, tc := range storeCases(t) {
		t.Run(tc.name, func(t *testing.T) {
			id, raw, err := tc.s.Create("acme", "ro", "pull")
			if err != nil {
				t.Fatalf("Create: %v", err)
			}
			info, ok := tc.s.Get("acme", id)
			if !ok {
				t.Fatal("Get: miss")
			}
			if len(info.Scopes) != 1 || info.Scopes[0] != "pull" {
				t.Fatalf("Get scopes = %v; want [pull]", info.Scopes)
			}
			lk, ok := tc.s.LookupKey(raw)
			if !ok {
				t.Fatal("LookupKey: miss")
			}
			if len(lk.Scopes) != 1 || lk.Scopes[0] != "pull" {
				t.Fatalf("LookupKey scopes = %v; want [pull]", lk.Scopes)
			}
			// Default scopes when omitted.
			_, _, err = tc.s.Create("acme", "full")
			if err != nil {
				t.Fatalf("Create default: %v", err)
			}
			list, err := tc.s.List("acme")
			if err != nil {
				t.Fatal(err)
			}
			var foundFull bool
			for _, k := range list {
				if k.Label == "full" {
					foundFull = true
					if len(k.Scopes) != 2 {
						t.Fatalf("default scopes = %v; want pull+push", k.Scopes)
					}
				}
			}
			if !foundFull {
				t.Fatal("full key missing from list")
			}
		})
	}
}
