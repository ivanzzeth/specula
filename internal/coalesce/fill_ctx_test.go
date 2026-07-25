package coalesce_test

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ivanzzeth/specula/internal/coalesce"
)

// TestFillContext_SurvivesParentCancel is the cold-fill contract: when
// containerd gives up waiting for response headers, the request ctx dies, but
// the fill must keep running so quarantine/CAS can finish for the next pull.
func TestFillContext_SurvivesParentCancel(t *testing.T) {
	parent, cancelParent := context.WithCancel(context.Background())
	fill, cancelFill := coalesce.FillContext(parent, time.Second)
	defer cancelFill()

	cancelParent()
	require.Error(t, parent.Err())
	require.NoError(t, fill.Err(), "fill ctx must outlive the HTTP request cancel")

	select {
	case <-fill.Done():
		t.Fatal("fill ctx must not be canceled by parent cancel")
	case <-time.After(30 * time.Millisecond):
	}
}

func TestFillContext_HonoursOwnTimeout(t *testing.T) {
	fill, cancel := coalesce.FillContext(context.Background(), 40*time.Millisecond)
	defer cancel()
	select {
	case <-fill.Done():
		assert.ErrorIs(t, fill.Err(), context.DeadlineExceeded)
	case <-time.After(2 * time.Second):
		t.Fatal("fill ctx did not expire")
	}
}
