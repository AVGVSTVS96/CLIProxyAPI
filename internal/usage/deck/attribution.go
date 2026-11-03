package deck

import (
	"math"
	"sort"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/entrypoint"
)

const (
	fiveHour = "fiveHour"
	week     = "week"
	// Readings of one window report its reset a few milliseconds apart.
	sameReset = 10 * 60_000
)

var limitKinds = []string{fiveHour, week}

var periods = map[string]float64{fiveHour: 5 * 3_600_000, week: 7 * 86_400_000}

type rise struct{ at, points float64 }

// limitWindow is one account's limit window as its readings saw it.
type limitWindow struct {
	account    string
	kind       string
	resetsAt   float64
	since      float64
	until      float64
	sampled    float64
	unassigned []rise
}

// limitCharge is the points one request was charged, per kind, and the window
// that charged them.
type limitCharge struct {
	account string
	points  [2]float64
	window  [2]*limitWindow
}

func kindIndex(kind string) int {
	if kind == fiveHour {
		return 0
	}
	return 1
}

// attributeLimits splits every rise of an account's limit across the requests
// that caused it, weighted by API cost, and for the five-hour limit also by the
// Agent SDK factor, since that meter charges Agent SDK tokens sdkFactor times
// more. Per window, charges plus unassigned points equal the sampled rise exactly.
func attributeLimits(readings []LimitReading, accountOf func(*prepared) string, events []prepared, sdkFactor float64) ([]*limitWindow, []*limitCharge) {
	// No window reaches further back than a week before the first reading.
	earliest := math.Inf(1)
	for _, row := range readings {
		earliest = math.Min(earliest, row.At-periods[week])
	}
	bound := map[string][]int{}
	for i := range events {
		if events[i].millis < earliest {
			continue
		}
		if account := accountOf(&events[i]); account != "" {
			bound[account] = append(bound[account], i)
		}
	}
	for _, list := range bound {
		sort.SliceStable(list, func(a, b int) bool { return events[list[a]].millis < events[list[b]].millis })
	}

	charges := make([]*limitCharge, len(events))
	var windows []*limitWindow
	charge := func(window *limitWindow, from, to, points float64) {
		window.sampled += points
		list := bound[window.account]
		low := sort.Search(len(list), func(i int) bool { return events[list[i]].millis > from })
		cost := func(e *prepared) float64 {
			if window.kind == fiveHour {
				return e.CostUSD * entrypoint.Weight(e.Entrypoint, sdkFactor)
			}
			return e.CostUSD
		}
		end, weight := low, 0.0
		for ; end < len(list) && events[list[end]].millis <= to; end++ {
			weight += cost(&events[list[end]])
		}
		if weight <= 0 {
			window.unassigned = append(window.unassigned, rise{at: to, points: points})
			return
		}
		kind := kindIndex(window.kind)
		for _, index := range list[low:end] {
			share := charges[index]
			if share == nil {
				share = &limitCharge{account: window.account}
				charges[index] = share
			}
			share.points[kind] += points * cost(&events[index]) / weight
			share.window[kind] = window
		}
	}

	var accounts []string
	byAccount := map[string][]LimitReading{}
	for _, row := range readings {
		if _, seen := byAccount[row.Account]; !seen {
			accounts = append(accounts, row.Account)
		}
		byAccount[row.Account] = append(byAccount[row.Account], row)
	}
	for _, account := range accounts {
		rows := sortedReadings(byAccount[account])
		monitoredSince := rows[0].At
		manualResets := manualResetsOf(rows)
		for _, kind := range limitKinds {
			// Rises are charged over the time since the previous reading of this kind,
			// so no request is charged twice, even when readings return to an earlier reset.
			type open struct {
				window *limitWindow
				peak   float64
			}
			var opened []*open
			var current *open
			last, manualResetAt := 0.0, 0.0
			for _, row := range rows {
				value := row.value(kind)
				if value == nil {
					continue
				}
				resetPtr := row.resetsAt(kind)
				if resetPtr == nil && current != nil {
					resetPtr = &current.window.resetsAt
				}
				if resetPtr == nil {
					continue
				}
				reset := *resetPtr
				if manualResets[row.At] {
					manualResetAt = row.At
				}
				current = nil
				for _, candidate := range opened {
					if candidate.window.since >= manualResetAt && math.Abs(reset-candidate.window.resetsAt) <= sameReset {
						current = candidate
						break
					}
				}
				if current == nil {
					window := &limitWindow{account: account, kind: kind, resetsAt: reset, since: row.At, until: row.At}
					current = &open{window: window, peak: *value}
					opened = append(opened, current)
					windows = append(windows, window)
					start := reset - periods[kind]
					// A window opened while monitored rose from zero; otherwise its first
					// reading is a baseline nothing logged here can explain.
					if *value > 0 && start >= monitoredSince && start >= last && start < row.At {
						charge(window, start, row.At, *value)
					} else if *value > 0 {
						window.sampled += *value
						window.unassigned = append(window.unassigned, rise{at: row.At, points: *value})
					}
				} else if *value > current.peak {
					charge(current.window, last, row.At, *value-current.peak)
					current.peak = *value
				}
				last = row.At
				current.window.until = math.Max(current.window.until, row.At)
			}
		}
	}
	return windows, charges
}

// manualResetsOf finds manual resets: both counters cleared without their
// scheduled reset times moving. The following reading must stay below the old
// week value, so one bad sample cannot reset accounting.
func manualResetsOf(rows []LimitReading) map[float64]bool {
	resets := map[float64]bool{}
	for i, row := range rows {
		if i == 0 || row.Week == nil || *row.Week != 0 || row.FiveHour == nil || *row.FiveHour != 0 {
			continue
		}
		before := rows[i-1]
		if before.Week == nil || *before.Week == 0 || before.FiveHour == nil || *before.FiveHour == 0 {
			continue
		}
		for _, next := range rows[i+1:] {
			if next.Week != nil {
				if *next.Week < *before.Week {
					resets[row.At] = true
				}
				break
			}
		}
	}
	return resets
}
