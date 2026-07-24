package verify

import (
	"bytes"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"os"
	"strings"
	"time"
)

// TrustedRoot is a parsed Sigstore trusted_root.json (v0.1 / v0.2 media types).
// Specula uses certificate_authorities as Fulcio trust anchors for offline
// keyless-style verification (leaf cert + signature). Transparency logs are
// loaded for diagnostics but NOT consulted while cosign.tlog remains false.
type TrustedRoot struct {
	MediaType string
	// Roots is the CertPool of Fulcio (and intermediate) CAs from the file.
	Roots *x509.CertPool
	// Authorities is the raw CA metadata (URI + validity) for messages/tests.
	Authorities []TrustedCA
	// TLogCount is how many Rekor/CT log entries were present (informational).
	TLogCount int
}

// TrustedCA is one certificate authority entry from trusted_root.json.
type TrustedCA struct {
	URI      string
	ValidFor TimeRange
	Certs    []*x509.Certificate
}

// TimeRange mirrors the trusted_root validFor object.
type TimeRange struct {
	Start time.Time
	End   time.Time // zero = open-ended
}

// trustedRootFile is the JSON shape of application/vnd.dev.sigstore.trustedroot*.
type trustedRootFile struct {
	MediaType             string             `json:"mediaType"`
	CertificateAuthorities []caJSON          `json:"certificateAuthorities"`
	TLogs                 []json.RawMessage  `json:"tlogs"`
	CTLogs                []json.RawMessage  `json:"ctlogs"`
}

type caJSON struct {
	URI       string `json:"uri"`
	CertChain struct {
		Certificates []struct {
			RawBytes string `json:"rawBytes"` // base64 DER
		} `json:"certificates"`
	} `json:"certChain"`
	ValidFor struct {
		Start string `json:"start"`
		End   string `json:"end"`
	} `json:"validFor"`
}

// LoadTrustedRoot reads and parses a Sigstore trusted_root.json from path.
func LoadTrustedRoot(path string) (*TrustedRoot, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("trusted_root: read %q: %w", path, err)
	}
	return ParseTrustedRoot(data)
}

// ParseTrustedRoot parses trusted_root JSON bytes.
func ParseTrustedRoot(data []byte) (*TrustedRoot, error) {
	var doc trustedRootFile
	if err := json.Unmarshal(data, &doc); err != nil {
		return nil, fmt.Errorf("trusted_root: parse JSON: %w", err)
	}
	mt := strings.TrimSpace(doc.MediaType)
	if mt != "" && !strings.Contains(mt, "trustedroot") {
		return nil, fmt.Errorf("trusted_root: unexpected mediaType %q", mt)
	}
	if len(doc.CertificateAuthorities) == 0 {
		return nil, fmt.Errorf("trusted_root: no certificateAuthorities (need Fulcio CA material)")
	}

	pool := x509.NewCertPool()
	var authorities []TrustedCA
	for i, ca := range doc.CertificateAuthorities {
		if len(ca.CertChain.Certificates) == 0 {
			return nil, fmt.Errorf("trusted_root: certificateAuthorities[%d] has empty certChain", i)
		}
		var certs []*x509.Certificate
		for j, c := range ca.CertChain.Certificates {
			der, err := base64.StdEncoding.DecodeString(strings.TrimSpace(c.RawBytes))
			if err != nil {
				return nil, fmt.Errorf("trusted_root: certificateAuthorities[%d].cert[%d] base64: %w", i, j, err)
			}
			cert, err := x509.ParseCertificate(der)
			if err != nil {
				return nil, fmt.Errorf("trusted_root: certificateAuthorities[%d].cert[%d] DER: %w", i, j, err)
			}
			certs = append(certs, cert)
			pool.AddCert(cert)
		}
		tr := TimeRange{}
		if ca.ValidFor.Start != "" {
			t, err := time.Parse(time.RFC3339, ca.ValidFor.Start)
			if err != nil {
				t, err = time.Parse(time.RFC3339Nano, ca.ValidFor.Start)
			}
			if err != nil {
				return nil, fmt.Errorf("trusted_root: certificateAuthorities[%d].validFor.start: %w", i, err)
			}
			tr.Start = t.UTC()
		}
		if ca.ValidFor.End != "" {
			t, err := time.Parse(time.RFC3339, ca.ValidFor.End)
			if err != nil {
				t, err = time.Parse(time.RFC3339Nano, ca.ValidFor.End)
			}
			if err != nil {
				return nil, fmt.Errorf("trusted_root: certificateAuthorities[%d].validFor.end: %w", i, err)
			}
			tr.End = t.UTC()
		}
		authorities = append(authorities, TrustedCA{
			URI:      ca.URI,
			ValidFor: tr,
			Certs:    certs,
		})
	}

	return &TrustedRoot{
		MediaType:   mt,
		Roots:       pool,
		Authorities: authorities,
		TLogCount:   len(doc.TLogs) + len(doc.CTLogs),
	}, nil
}

// VerifyCertificateChain checks that leaf (and optional PEM chain) chains to a
// Fulcio CA in the trusted root. Uses CurrentTime for validity windows.
func (tr *TrustedRoot) VerifyCertificateChain(leaf *x509.Certificate, chainPEM []byte, now time.Time) error {
	if tr == nil || tr.Roots == nil {
		return fmt.Errorf("trusted_root: nil roots")
	}
	if leaf == nil {
		return fmt.Errorf("trusted_root: nil leaf certificate")
	}
	intermediates := x509.NewCertPool()
	for _, c := range parsePEMCerts(chainPEM) {
		intermediates.AddCert(c)
	}
	// Also add non-root certs from each authority chain as intermediates.
	for _, ca := range tr.Authorities {
		for _, c := range ca.Certs {
			if !c.IsCA || c.Subject.String() != c.Issuer.String() {
				intermediates.AddCert(c)
			}
		}
	}
	if now.IsZero() {
		now = time.Now().UTC()
	}
	opts := x509.VerifyOptions{
		Roots:         tr.Roots,
		Intermediates: intermediates,
		CurrentTime:   now,
		KeyUsages:    []x509.ExtKeyUsage{x509.ExtKeyUsageCodeSigning, x509.ExtKeyUsageAny},
	}
	if _, err := leaf.Verify(opts); err != nil {
		return fmt.Errorf("trusted_root: certificate chain verification failed: %w", err)
	}
	return nil
}

func parsePEMCerts(pemBytes []byte) []*x509.Certificate {
	var out []*x509.Certificate
	rest := pemBytes
	for {
		var block *pem.Block
		block, rest = pem.Decode(rest)
		if block == nil {
			break
		}
		if block.Type != "CERTIFICATE" {
			continue
		}
		c, err := x509.ParseCertificate(block.Bytes)
		if err != nil {
			continue
		}
		out = append(out, c)
	}
	return out
}

func parseLeafCertPEM(pemOrDER []byte) (*x509.Certificate, error) {
	pemOrDER = bytes.TrimSpace(pemOrDER)
	if len(pemOrDER) == 0 {
		return nil, fmt.Errorf("empty certificate")
	}
	if block, _ := pem.Decode(pemOrDER); block != nil && block.Type == "CERTIFICATE" {
		return x509.ParseCertificate(block.Bytes)
	}
	if c, err := x509.ParseCertificate(pemOrDER); err == nil {
		return c, nil
	}
	certs := parsePEMCerts(pemOrDER)
	if len(certs) == 0 {
		return nil, fmt.Errorf("no CERTIFICATE PEM/DER found")
	}
	return certs[0], nil
}
