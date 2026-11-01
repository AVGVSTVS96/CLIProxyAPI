package usage

import (
	"context"
	"os"
	"runtime/pprof"
	"testing"
)

func TestWarmCPU(t *testing.T) {
	out := os.Getenv("USAGE_CPU_PROFILE")
	if out == "" {
		t.Skip()
	}
	c := NewCollector(Sources{DeviceID: "x"}, NewPricing(""))
	c.Collect(context.Background())
	f, _ := os.Create(out)
	_ = pprof.StartCPUProfile(f)
	for range 5 {
		c.Collect(context.Background())
	}
	pprof.StopCPUProfile()
	f.Close()
}
