package usage

import (
	"context"
	"os"
	"runtime"
	"runtime/pprof"
	"testing"
	"time"
)

// TestMemory measures a server's steady state: USAGE_MEM_STATE=<copy of a state dir> go test -run TestMemory
func TestMemory(t *testing.T) {
	dir := os.Getenv("USAGE_MEM_STATE")
	if dir == "" {
		t.Skip("USAGE_MEM_STATE not set")
	}
	report := func(label string) {
		runtime.GC()
		var m runtime.MemStats
		runtime.ReadMemStats(&m)
		t.Logf("%s: heapInuse=%dMB heapSys=%dMB sys=%dMB", label, m.HeapInuse>>20, m.HeapSys>>20, m.Sys>>20)
	}
	pricing := NewPricing(dir)
	store, err := OpenStore(dir, "e1b2a2af-df21-4a69-b598-53e1a403e671", "server", pricing)
	if err != nil {
		t.Fatal(err)
	}
	report("store loaded")
	c := NewCollector(Sources{DeviceID: "e1b2a2af-df21-4a69-b598-53e1a403e671"}, pricing)
	started := time.Now()
	col := c.Collect(context.Background())
	t.Logf("collect %s", time.Since(started))
	started = time.Now()
	if _, err := store.Apply(Batch{Device: Device{ID: "e1b2a2af-df21-4a69-b598-53e1a403e671"}, Events: col.Events, DeletedIDs: col.RetractedIDs}, false); err != nil {
		t.Fatal(err)
	}
	t.Logf("apply %s", time.Since(started))
	col = Collection{}
	report("collected")
	if out := os.Getenv("USAGE_MEM_PROFILE"); out != "" {
		f, _ := os.Create(out + ".collected")
		_ = pprof.WriteHeapProfile(f)
		f.Close()
	}
	started = time.Now()
	events := store.Events()
	t.Logf("events %d in %s", len(events), time.Since(started))
	report("snapshot")
	for range 3 {
		started = time.Now()
		col = c.Collect(context.Background())
		collectTime := time.Since(started)
		_, _ = store.Apply(Batch{Device: Device{ID: "e1b2a2af-df21-4a69-b598-53e1a403e671"}, Events: col.Events, DeletedIDs: col.RetractedIDs}, false)
		t.Logf("warm collect %s, total %s", collectTime, time.Since(started))
	}
	report("warm")
	runtime.KeepAlive(c)
	if out := os.Getenv("USAGE_MEM_PROFILE"); out != "" {
		f, _ := os.Create(out)
		_ = pprof.WriteHeapProfile(f)
		f.Close()
	}
	runtime.KeepAlive(events)
	runtime.KeepAlive(c)
}
