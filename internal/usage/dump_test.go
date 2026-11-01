package usage

import (
	"bufio"
	"context"
	"encoding/json"
	"os"
	"runtime"
	"testing"
	"time"
)

// TestDumpCollection writes this machine's collection as NDJSON for parity checks against
// token-tracker: USAGE_DUMP=<file> [USAGE_DUMP_DEVICE=<id>] [USAGE_DUMP_CACHE=<dir>] go test -run TestDumpCollection
func TestDumpCollection(t *testing.T) {
	out := os.Getenv("USAGE_DUMP")
	if out == "" {
		t.Skip("USAGE_DUMP not set")
	}
	pricing := NewPricing(os.Getenv("USAGE_DUMP_PRICING"))
	c := NewCollector(Sources{DeviceID: os.Getenv("USAGE_DUMP_DEVICE")}, pricing)
	started := time.Now()
	collection := c.Collect(context.Background())
	elapsed := time.Since(started)
	var m runtime.MemStats
	runtime.ReadMemStats(&m)
	t.Logf("events=%d retracted=%d elapsed=%s heapInuse=%dMB totalAlloc=%dMB sys=%dMB warnings=%v",
		len(collection.Events), len(collection.RetractedIDs), elapsed, m.HeapInuse>>20, m.TotalAlloc>>20, m.Sys>>20, collection.Warnings)
	started = time.Now()
	collection = c.Collect(context.Background())
	t.Logf("warm pass: %s", time.Since(started))
	file, err := os.Create(out)
	if err != nil {
		t.Fatal(err)
	}
	w := bufio.NewWriter(file)
	for _, e := range collection.Events {
		data, _ := json.Marshal(e)
		w.Write(append(data, '\n'))
	}
	w.Flush()
	file.Close()
	retracted, _ := json.Marshal(collection.RetractedIDs)
	_ = os.WriteFile(out+".retracted.json", retracted, 0o644)
}
