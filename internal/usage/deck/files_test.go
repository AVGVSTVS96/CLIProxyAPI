package deck

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
)

func TestFilesServeThePoolsStateFromAnOffsetAndNameWhatGrew(t *testing.T) {
	state, home := t.TempDir(), t.TempDir()
	receipts := filepath.Join(state, "receipts.ndjson")
	if err := os.WriteFile(receipts, []byte("{\"a\":1}\n{\"b\":2}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	var cfg config.Config
	cfg.Routing.Pool.StateDir = state
	cfg.Usage.Receipts = receipts
	cfg.Usage.Hex = filepath.Join(home, "hex")
	cfg.Usage.Claude = filepath.Join(home, "claude")
	d := New(Options{Config: cfg})
	defer d.Close()
	gin.SetMode(gin.TestMode)
	engine := gin.New()
	d.Register(engine.Group("/deck"))
	get := func(name, ranges string) *httptest.ResponseRecorder {
		request := httptest.NewRequest(http.MethodGet, "/deck/files/"+name, nil)
		if ranges != "" {
			request.Header.Set("Range", ranges)
		}
		response := httptest.NewRecorder()
		engine.ServeHTTP(response, request)
		return response
	}

	if r := get("receipts.ndjson", "bytes=8-"); r.Code != http.StatusPartialContent || r.Body.String() != "{\"b\":2}\n" {
		t.Fatalf("from an offset: %d %q", r.Code, r.Body.String())
	}
	if r := get("receipts.ndjson", "bytes=16-"); r.Code != http.StatusRequestedRangeNotSatisfiable || r.Header().Get("Content-Range") != "bytes */16" {
		t.Fatalf("at the end: %d %q", r.Code, r.Header().Get("Content-Range"))
	}
	if r := get("credits.json", ""); r.Code != http.StatusOK || r.Body.Len() != 0 {
		t.Fatalf("a file the pool hasn't written: %d %q", r.Code, r.Body.String())
	}
	if r := get("config.yaml", ""); r.Code != http.StatusNotFound {
		t.Fatalf("a file the pool doesn't serve: %d", r.Code)
	}

	events := d.stream.subscribe()
	<-events
	time.Sleep(100 * time.Millisecond)
	file, err := os.OpenFile(receipts, os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = file.WriteString("{\"c\":3}\n")
	_ = file.Close()
	deadline := time.After(5 * time.Second)
	for {
		select {
		case frame := <-events:
			if strings.Contains(string(frame), `"tags":["receipts.ndjson"]`) {
				return
			}
		case <-deadline:
			t.Fatal("no stale event named receipts.ndjson")
		}
	}
}
