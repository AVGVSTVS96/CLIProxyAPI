package deck

import (
	"math"
	"strings"
	"time"
	"unicode"
)

// Range is one of the time ranges the Usage page offers.
type Range string

const (
	Today     Range = "today"
	Yesterday Range = "yesterday"
	Week      Range = "7d"
	Month     Range = "30d"
	HalfYear  Range = "6m"
)

// Ranges lists every range in the order the page shows them.
var Ranges = []Range{Today, Yesterday, Week, Month, HalfYear}

type timeframe struct {
	start, previousStart, end time.Time
}

func startOfDay(t time.Time) time.Time {
	y, m, d := t.Date()
	return time.Date(y, m, d, 0, 0, 0, 0, t.Location())
}

func addDays(t time.Time, days int) time.Time {
	y, m, d := t.Date()
	return time.Date(y, m, d+days, t.Hour(), t.Minute(), t.Second(), t.Nanosecond(), t.Location())
}

// addMonths keeps the day of month, clamped to the target month's last day.
func addMonths(t time.Time, months int) time.Time {
	y, m, d := t.Date()
	first := time.Date(y, m+time.Month(months), 1, 0, 0, 0, 0, t.Location())
	last := time.Date(first.Year(), first.Month()+1, 0, 0, 0, 0, 0, t.Location()).Day()
	return time.Date(first.Year(), first.Month(), min(d, last), t.Hour(), t.Minute(), t.Second(), t.Nanosecond(), t.Location())
}

// usageTimeframe is token-tracker's: calendar days in the zone, and 7d and 30d
// counted back from now.
func usageTimeframe(r Range, zone *time.Location, now time.Time) timeframe {
	current := now.In(zone)
	today := startOfDay(current)
	end := startOfDay(addDays(today, 1))
	switch r {
	case Today:
		return timeframe{start: today, previousStart: addDays(today, -1), end: end}
	case Yesterday:
		start := addDays(today, -1)
		return timeframe{start: start, previousStart: addDays(start, -1), end: today}
	case HalfYear:
		start := addMonths(end, -6)
		return timeframe{start: start, previousStart: addMonths(start, -6), end: end}
	}
	days := 30
	if r == Week {
		days = 7
	}
	start := addDays(current, -days)
	return timeframe{start: start, previousStart: addDays(start, -days), end: end}
}

// bucketStarts are the starts of each chart bucket in the range: hours step in
// exact time, days and weeks on the calendar.
func bucketStarts(r Range, frame timeframe) (string, []int64) {
	at := frame.start
	if r == Week || r == Month {
		at = startOfDay(at)
	}
	size, step := "1h", func(t time.Time) time.Time { return t.Add(time.Hour) }
	switch r {
	case Week:
		size, step = "4h", func(t time.Time) time.Time { return t.Add(4 * time.Hour) }
	case Month:
		size, step = "1d", func(t time.Time) time.Time { return addDays(t, 1) }
	case HalfYear:
		size, step = "1w", func(t time.Time) time.Time { return addDays(t, 7) }
	}
	var starts []int64
	for end := frame.end.UnixMilli(); at.UnixMilli() < end; at = step(at) {
		starts = append(starts, at.UnixMilli())
	}
	return size, starts
}

var timestampLayouts = []string{time.RFC3339Nano, "2006-01-02T15:04:05.999999999", "2006-01-02"}

// parseMillis reads a timestamp the way JavaScript's Date.parse does for ISO
// strings, NaN when it can't.
func parseMillis(value string) float64 {
	for _, layout := range timestampLayouts {
		loc := time.UTC
		if layout == timestampLayouts[1] {
			loc = time.Local
		}
		if t, err := time.ParseInLocation(layout, value, loc); err == nil {
			return float64(t.UnixMilli())
		}
	}
	return math.NaN()
}

var namespaces = map[string]string{
	"openai": "openai", "anthropic": "anthropic", "xai": "xai", "x-ai": "xai", "google": "google",
	"gemini": "google", "google_genai": "google", "vertex_ai": "google", "vertex-ai": "google",
	"deepseek": "deepseek", "qwen": "alibaba", "alibaba": "alibaba", "dashscope": "alibaba",
	"mistral": "mistral", "mistralai": "mistral", "meta": "meta", "meta-llama": "meta",
	"moonshot": "moonshot", "moonshotai": "moonshot", "zai": "zai", "z-ai": "zai", "zhipu": "zai",
	"cohere": "cohere", "amazon": "amazon", "aws": "amazon", "bedrock": "amazon",
	"bedrock_converse": "amazon", "bedrock_mantle": "amazon", "azure": "azure", "azure_ai": "azure",
	"openrouter": "openrouter", "together": "together", "together_ai": "together", "groq": "groq",
	"perplexity": "perplexity", "nvidia": "nvidia", "huggingface": "huggingface",
	"fireworks": "fireworks", "fireworks_ai": "fireworks",
}

var families = []struct {
	name     string
	prefixes []string
}{
	{"openai", []string{"gpt-", "chatgpt-", "o1-", "o3-", "o4-", "codex-mini-"}},
	{"anthropic", []string{"claude-"}},
	{"google", []string{"gemini-", "gemma-"}},
	{"xai", []string{"grok-"}},
	{"deepseek", []string{"deepseek-"}},
	{"alibaba", []string{"qwen"}},
	{"mistral", []string{"mistral-", "mixtral-", "codestral-", "magistral-", "ministral-", "devstral-"}},
	{"meta", []string{"llama-", "llama2-", "llama3-", "llama4-", "muse-"}},
	{"moonshot", []string{"kimi-", "moonshot-"}},
	{"zai", []string{"glm-"}},
	{"cohere", []string{"command-r", "command-a"}},
	{"amazon", []string{"amazon.nova-"}},
}

// provider names the company behind a model id.
func provider(model string) string {
	normalized := strings.ToLower(strings.TrimSpace(model))
	if namespace, _, ok := strings.Cut(normalized, "/"); ok {
		if name, known := namespaces[namespace]; known {
			return name
		}
		return "unknown"
	}
	for _, family := range families {
		for _, prefix := range family.prefixes {
			if strings.HasPrefix(normalized, prefix) {
				return family.name
			}
		}
	}
	switch normalized {
	case "o1", "o3", "o4":
		return "openai"
	}
	return "unknown"
}

// asciiOrder is the root collation's order of ASCII punctuation, symbols and
// digits, which JavaScript's localeCompare sorts before letters.
const asciiOrder = " _-,;:!?.'\"()[]{}@*/\\&#%`^+<=>|~$0123456789"

func primary(r rune) int {
	if i := strings.IndexRune(asciiOrder, r); i >= 0 {
		return i
	}
	if r < unicode.MaxASCII && unicode.IsLetter(r) {
		return len(asciiOrder) + int(unicode.ToLower(r)-'a')
	}
	return len(asciiOrder) + 26 + int(r)
}

// localeCompare orders strings the way JavaScript's localeCompare does for the
// ASCII ids sessions use: letters ignore case first, then lowercase leads.
func localeCompare(a, b string) int {
	ra, rb := []rune(a), []rune(b)
	for i := 0; i < len(ra) && i < len(rb); i++ {
		if pa, pb := primary(ra[i]), primary(rb[i]); pa != pb {
			return pa - pb
		}
	}
	if len(ra) != len(rb) {
		return len(ra) - len(rb)
	}
	for i := range ra {
		if ra[i] != rb[i] {
			if unicode.IsLower(ra[i]) {
				return -1
			}
			return 1
		}
	}
	return 0
}
