//go:build unix && !darwin

package perf

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestMergeGpuStatChannels(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	nvidia := make(chan []GpuStat, 1)
	amd := make(chan []GpuStat, 1)
	out := mergeGpuStatChannels(ctx, nvidia, amd)

	nvidia <- []GpuStat{{ID: 0, UUID: "GPU-nv", Name: "NVIDIA", GpuUtilPct: 10}}
	amd <- []GpuStat{
		{ID: 1, UUID: "0000:02:00.0", Name: "RX580", GpuUtilPct: 100},
		{ID: 3, UUID: "0000:03:00.0", Name: "RX580", GpuUtilPct: 0},
	}

	deadline := time.After(2 * time.Second)
	var got []GpuStat
	for len(got) < 3 {
		select {
		case snap := <-out:
			got = snap
		case <-deadline:
			t.Fatalf("timed out waiting for merged snapshot, last=%v", got)
		}
	}

	require.Len(t, got, 3)
	assert.Equal(t, "GPU-nv", got[0].UUID)
	assert.Equal(t, "0000:02:00.0", got[1].UUID)
	assert.Equal(t, "0000:03:00.0", got[2].UUID)

	cancel()
	close(nvidia)
	close(amd)
}
