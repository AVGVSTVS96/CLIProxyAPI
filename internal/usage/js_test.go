package usage

import (
	"math"
	"testing"
)

func TestJSNumber(t *testing.T) {
	for in, want := range map[float64]string{
		1e21: "1e+21", 1e-7: "1e-7", 0.000003: "0.000003", 123.456: "123.456", 0: "0", 5e-324: "5e-324",
		1.7976931348623157e308: "1.7976931348623157e+308", 100: "100", 0.30000000000000004: "0.30000000000000004",
		9007199254740994: "9007199254740994", 1.5e-6: "0.0000015", 123456789012345680000: "123456789012345680000",
	} {
		if got := jsNumber(in); got != want {
			t.Errorf("jsNumber(%v) = %s, want %s", in, got, want)
		}
	}
}

// Expected values are Bun's Date.parse results for the same strings.
func TestParseDate(t *testing.T) {
	for in, want := range map[string]float64{
		"2025-01-01T00:00:00.1239Z":      1735689600123,
		"2025-01-01T00:00:00.9999Z":      1735689600999,
		"2025-01-01T00:00Z":              1735689600000,
		"2025-01-01":                     1735689600000,
		"2025-01":                        1735689600000,
		"2025":                           1735689600000,
		"2025-01-01T00:00:00.5+02:00":    1735682400500,
		"2025-01-01T00:00:00+0200":       1735682400000,
		"2025-01-01 00:00:00Z":           1735689600000,
		"2025-01-01T24:00:00Z":           1735776000000,
		"2025-02-30T00:00:00Z":           1740873600000,
		"+002025-01-01T00:00:00Z":        1735689600000,
		"2025-01-01T00:00:00.123456789Z": 1735689600123,
		"Tue, 01 Jan 2025 00:00:00 GMT":  1735689600000,
		"2025-01-01t00:00:00z":           1735689600000,
		"2025-01-01T00:00:00.000-00:00":  1735689600000,
	} {
		if got := parseDate(in); got != want {
			t.Errorf("parseDate(%q) = %v, want %v", in, got, want)
		}
	}
	for _, in := range []string{"2025-01-01T00:00:60Z", "2025-01-01T00:00:00.Z", " 2025-01-01T00:00:00Z", "1e3"} {
		if got := parseDate(in); !math.IsNaN(got) {
			t.Errorf("parseDate(%q) = %v, want NaN", in, got)
		}
	}
}

func TestISOAndTrim(t *testing.T) {
	if got := isoMillis(1735689600123.9); got != "2025-01-01T00:00:00.123Z" {
		t.Fatal(got)
	}
	if isoMillis(0) != "" || isoMillis(9e15) != "" {
		t.Fatal("invalid dates must render empty")
	}
	if got := jsTrim(" \ufeff\u00a0\u2028x\u3000 "); got != "x" {
		t.Fatalf("%q", got)
	}
	if got := jsonArray("a\"b\n\u0001", 3, 1.5); got != `["a\"b\n\u0001",3,1.5]` {
		t.Fatal(got)
	}
}
