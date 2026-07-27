package cluster

import (
	"os"
	"testing"
)

// Live check against a real cluster: set SPECULA_LIVE_KUBECONFIG to run it.
// Skipped by default so it never touches CI.
func TestAutoPinNodeLive(t *testing.T) {
	kc := os.Getenv("SPECULA_LIVE_KUBECONFIG")
	if kc == "" {
		t.Skip("set SPECULA_LIVE_KUBECONFIG to run against a real cluster")
	}
	name, err := AutoPinNode(kc, "", DefaultRequestMi)
	t.Logf("pick=%q err=%v", name, err)
}
