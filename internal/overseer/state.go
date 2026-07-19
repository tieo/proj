package overseer

import (
	"encoding/json"
	"os"
	"path/filepath"
	"time"
)

// StateNoGoal marks a session the overseer declined to judge because on_goal
// mode is set and the session has no open /goal. The daemon assigns it; the
// judge never returns it, so it stays out of the four verdict states.
const StateNoGoal = "no_goal"

// SessionMemory is the overseer's carried state for one session between judges:
// the transcript signature at the last judge (so a session is judged once per
// stop, not every tick), its last verdict, and the nudge/notify bookkeeping.
type SessionMemory struct {
	Sig         string    `json:"sig"`
	Nudges      int       `json:"nudges"`
	Notified    bool      `json:"notified"`
	State       string    `json:"state"`        // last judged: working | done | stopped_short | blocked, or no_goal when not judged
	Goal        string    `json:"goal"`         // last inferred goal
	Reason      string    `json:"reason"`       // why the judge chose that state
	NextRecheck time.Time `json:"next_recheck"` // when to re-judge a blocked session even with no new work; zero = only on new work
}

// Memory is the overseer's durable state, kept in the scratch dir so a daemon
// restart does not re-nudge or re-judge sessions it already handled. LastLook is
// the last time any session was judged, shown in the status report.
type Memory struct {
	LastLook time.Time                `json:"last_look"`
	Sessions map[string]SessionMemory `json:"sessions"`
}

func memoryPath() string { return filepath.Join(ScratchDir(), "lookstate.json") }

// LoadMemory reads the overseer's state, or an empty one when none exists.
func LoadMemory() Memory {
	m := Memory{Sessions: map[string]SessionMemory{}}
	b, err := os.ReadFile(memoryPath())
	if err != nil {
		return m
	}
	_ = json.Unmarshal(b, &m)
	if m.Sessions == nil {
		m.Sessions = map[string]SessionMemory{}
	}
	return m
}

// Save writes the overseer's state back to the scratch dir.
func (m Memory) Save() error {
	if err := os.MkdirAll(ScratchDir(), 0o755); err != nil {
		return err
	}
	b, err := json.Marshal(m)
	if err != nil {
		return err
	}
	return os.WriteFile(memoryPath(), b, 0o644)
}

// Prune drops memory for sessions not in live, so the state cannot grow without
// bound as projects come and go.
func (m Memory) Prune(live map[string]bool) {
	for name := range m.Sessions {
		if !live[name] {
			delete(m.Sessions, name)
		}
	}
}

// Action is what a verdict calls for.
type Action int

const (
	ActNone   Action = iota // let it be (working, done, or already handled)
	ActNudge                // type the callout into the session to continue it
	ActNotify               // push the user: a decision the agent cannot make
)

// Decide maps a verdict and the session's current memory to the action to take
// and the memory to carry forward, without any side effect. It is the whole
// policy: nudge a session that stopped short with a path still open, up to
// maxNudges times; notify (not nudge) when a real user decision is needed; clear
// the stall memory once the session is working again or done. The Nudges count
// is advanced by the caller only on a nudge that actually sent (the composer
// guard can veto), so Decide leaves it untouched for ActNudge.
func Decide(v Verdict, m SessionMemory, maxNudges int) (Action, SessionMemory) {
	switch v.State {
	case "stopped_short":
		if v.NeedsUser {
			return ActNotify, m
		}
		if v.Callout == "" || m.Nudges >= maxNudges {
			return ActNone, m
		}
		return ActNudge, m
	case "blocked":
		if v.NeedsUser {
			return ActNotify, m
		}
		return ActNone, m
	default: // working, done: progress or terminal - clear the stall memory
		m.Nudges = 0
		m.Notified = false
		return ActNone, m
	}
}
