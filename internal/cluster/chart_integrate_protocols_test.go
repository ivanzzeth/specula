package cluster

// Render tests for the bootstrap integrate DaemonSet's --protocols default.
//
// WHY THIS EXISTS
// ---------------
// Iron law (chorei #23 / Specula delivery contract): node wiring via
// `bootstrap-node` / the integrate DaemonSet MUST apply DefaultProtocols
// (go,npm,pypi,oci,helm,git,apt,cargo,conda,hf). A chart default of
// `integrate.protocols: "oci"` silently narrowed every CN cluster to OCI-only
// client wiring even though Specula itself served all protocols — the exact
// "收窄交付" bug. This pins the rendered DaemonSet args to the full list so
// the default cannot regress to oci-only.

import (
	"strings"
	"testing"

	"github.com/ivanzzeth/specula/internal/integrate"
)

func TestChartDefaultIntegrateProtocolsAreFullDefaultProtocols(t *testing.T) {
	out := helmTemplate(t)
	wantFlag := "--protocols=" + strings.Join(integrate.DefaultProtocols, ",")
	if !strings.Contains(out, wantFlag) {
		t.Fatalf("integrate DaemonSet must wire full DefaultProtocols; want substring %q in helm template output", wantFlag)
	}
	// Guard the old bug: a bare `--protocols=oci` (no other protocols) must not
	// appear as the sole protocols arg. The full flag above already implies
	// this, but assert explicitly so a "oci,oci" or truncated render fails loud.
	if strings.Contains(out, "--protocols=oci\n") || strings.Contains(out, "--protocols=oci\r") {
		t.Fatalf("integrate DaemonSet still renders oci-only --protocols (law #23 regression)")
	}
}
