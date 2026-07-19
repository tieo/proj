package overseer

import (
	"bufio"
	"encoding/json"
	"os"
	"sort"
	"time"
)

// DayBudget is the overseer's daily token target: ~2% of the ~20M/day sonnet
// plan quota. Effective tokens summed over a day are reported against it.
const DayBudget = 400_000

// effective weights a look's usage into tokens billed against the plan: fresh
// input and output at full rate, cache reads at 0.1x, cache writes at ~1.25x.
func effective(input, output, cacheRead, cacheCreate int) int {
	return input + output + cacheRead/10 + cacheCreate*5/4
}

// Effective is effective tokens for a live look's usage.
func Effective(u Usage) int { return effective(u.Input, u.Output, u.CacheRead, u.CacheCreate) }

// UsageRecord is one logged look, read back from the usage JSONL.
type UsageRecord struct {
	At          time.Time
	Judged      int
	Input       int
	Output      int
	CacheRead   int
	CacheCreate int
}

// Effective is effective tokens for this look.
func (r UsageRecord) Effective() int {
	return effective(r.Input, r.Output, r.CacheRead, r.CacheCreate)
}

// ReadUsageLog returns every logged look, oldest first, skipping unparseable
// lines. Empty when the log does not exist yet.
func ReadUsageLog() []UsageRecord {
	f, err := os.Open(UsageLogPath())
	if err != nil {
		return nil
	}
	defer f.Close()
	var out []UsageRecord
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64*1024), 1<<20)
	for sc.Scan() {
		line := sc.Bytes()
		if len(line) == 0 {
			continue
		}
		var rec struct {
			TS          string `json:"ts"`
			Judged      int    `json:"judged"`
			Input       int    `json:"input"`
			Output      int    `json:"output"`
			CacheRead   int    `json:"cache_read"`
			CacheCreate int    `json:"cache_create"`
		}
		if json.Unmarshal(line, &rec) != nil {
			continue
		}
		at, _ := time.Parse(time.RFC3339, rec.TS)
		out = append(out, UsageRecord{at, rec.Judged, rec.Input, rec.Output, rec.CacheRead, rec.CacheCreate})
	}
	return out
}

// SessionMemory is the overseer's carried state for one session, exposed for the
// status report.
type SessionMemory struct {
	Name     string
	Nudges   int
	Notified bool
}

// ReadLookState returns the time of the last look and the per-session memory,
// sorted by name. A zero time means no look has run yet.
func ReadLookState() (time.Time, []SessionMemory) {
	st := loadLookState()
	out := make([]SessionMemory, 0, len(st.Sessions))
	for name, sl := range st.Sessions {
		out = append(out, SessionMemory{Name: name, Nudges: sl.Nudges, Notified: sl.Notified})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return st.LastLook, out
}

// TodayUsage sums looks and effective tokens for records on the same calendar
// day as now (local time).
func TodayUsage(recs []UsageRecord, now time.Time) (looks, eff int) {
	y, m, d := now.Date()
	for _, r := range recs {
		ry, rm, rd := r.At.Local().Date()
		if ry == y && rm == m && rd == d {
			looks++
			eff += r.Effective()
		}
	}
	return looks, eff
}
