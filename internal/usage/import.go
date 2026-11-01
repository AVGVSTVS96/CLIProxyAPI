package usage

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
)

// ImportTokenTracker copies the history token-tracker stored for other machines (from its
// data directory's usage.sqlite) into the store, with the costs those machines reported,
// filling only records the store lacks. It also adopts token-tracker's pricing-state.json
// (catalog and saved rules) when the store has none. Run it while the server is stopped.
func ImportTokenTracker(ctx context.Context, cfg config.UsageConfig, dataDir string) (map[string]Result, error) {
	m, err := openMachine(cfg)
	if err != nil {
		return nil, err
	}
	unlock, err := lockState(m.stateDir)
	if err != nil {
		return nil, err
	}
	defer unlock()
	dataDir = expandHome(dataDir)
	database := filepath.Join(dataDir, "usage.sqlite")
	if _, err = os.Stat(database); err != nil {
		return nil, err
	}
	if _, err = os.Stat(m.pricing.path()); errors.Is(err, os.ErrNotExist) {
		if data, readErr := os.ReadFile(filepath.Join(dataDir, pricingFile)); readErr == nil {
			if err = writeFileAtomic(m.pricing.path(), data); err != nil {
				return nil, err
			}
			m.pricing.reload()
		}
	}
	store, err := OpenStore(m.stateDir, m.id, m.label, m.pricing)
	if err != nil {
		return nil, err
	}
	devices := map[string]Device{}
	err = sqliteRows(ctx, database, "SELECT id, name, last_seen AS lastSeen FROM devices", func(row json.RawMessage) error {
		var d struct {
			ID, Name string
			LastSeen *string
		}
		if err := json.Unmarshal(row, &d); err != nil {
			return err
		}
		device := Device{ID: d.ID, Label: d.Name}
		if d.LastSeen != nil {
			if ms := parseDate(*d.LastSeen); !math.IsNaN(ms) {
				synced := int64(ms)
				device.SyncedAt = &synced
			}
		}
		devices[d.ID] = device
		return nil
	})
	if err != nil {
		return nil, err
	}
	results := map[string]Result{}
	var pending []Event
	current := ""
	flush := func() error {
		if len(pending) == 0 {
			return nil
		}
		result, err := store.Apply(Batch{Device: devices[current], Events: pending}, true)
		total := results[current]
		total.Accepted += result.Accepted
		results[current] = total
		pending = pending[:0]
		return err
	}
	query := "SELECT device_id AS d, payload AS p FROM usage_events WHERE device_id <> '" +
		strings.ReplaceAll(m.id, "'", "''") + "' ORDER BY device_id, timestamp"
	err = sqliteRows(ctx, database, query, func(row json.RawMessage) error {
		var r struct{ D, P string }
		if err := json.Unmarshal(row, &r); err != nil {
			return err
		}
		var e Event
		if err := json.Unmarshal([]byte(r.P), &e); err != nil {
			return fmt.Errorf("record on %s: %w", r.D, err)
		}
		if r.D != current || len(pending) >= 5000 {
			if err := flush(); err != nil {
				return err
			}
			current = r.D
			if _, ok := devices[current]; !ok {
				devices[current] = Device{ID: current, Label: current}
			}
		}
		pending = append(pending, e)
		return nil
	})
	if err == nil {
		err = flush()
	}
	return results, err
}

// sqliteRows streams a read-only query's rows as JSON objects.
func sqliteRows(ctx context.Context, database, query string, row func(json.RawMessage) error) error {
	cmd := exec.CommandContext(ctx, "sqlite3", "-readonly", "-json", database, query)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	var stderr strings.Builder
	cmd.Stderr = &stderr
	if err = cmd.Start(); err != nil {
		return err
	}
	decodeErr := decodeRows(bufio.NewReaderSize(stdout, 1<<20), row)
	if decodeErr != nil {
		_ = cmd.Process.Kill()
	}
	if waitErr := cmd.Wait(); decodeErr == nil && waitErr != nil {
		return fmt.Errorf("sqlite3: %v %s", waitErr, strings.TrimSpace(stderr.String()))
	}
	return decodeErr
}

func decodeRows(r io.Reader, row func(json.RawMessage) error) error {
	decoder := json.NewDecoder(r)
	if _, err := decoder.Token(); err != nil {
		if errors.Is(err, io.EOF) {
			return nil
		}
		return err
	}
	for decoder.More() {
		var raw json.RawMessage
		if err := decoder.Decode(&raw); err != nil {
			return err
		}
		if err := row(raw); err != nil {
			return err
		}
	}
	return nil
}
