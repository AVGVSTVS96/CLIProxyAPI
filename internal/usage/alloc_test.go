package usage

import (
	"context"
	"os"
	"runtime"
	"runtime/pprof"
	"testing"
)

func TestColdAllocs(t *testing.T) {
	out := os.Getenv("USAGE_ALLOC_PROFILE")
	if out == "" {
		t.Skip()
	}
	runtime.MemProfileRate = 64 * 1024
	c := NewCollector(Sources{DeviceID: "x"}, NewPricing(""))
	c.Collect(context.Background())
	f, _ := os.Create(out)
	_ = pprof.Lookup("allocs").WriteTo(f, 0)
	f.Close()
}
