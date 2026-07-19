package overseer

import "testing"

func TestDecide(t *testing.T) {
	const maxNudges = 3
	cases := []struct {
		name    string
		v       Verdict
		m       SessionMemory
		want    Action
		nudges  int  // expected Nudges in the returned memory
		cleared bool // expected Notified == false in the returned memory
	}{
		{
			name: "stopped short with a callout nudges",
			v:    Verdict{State: "stopped_short", Callout: "keep going"},
			want: ActNudge,
		},
		{
			name:   "stopped short at the nudge limit does nothing",
			v:      Verdict{State: "stopped_short", Callout: "keep going"},
			m:      SessionMemory{Nudges: maxNudges},
			want:   ActNone,
			nudges: maxNudges, // preserved, not reset
		},
		{
			name: "stopped short without a callout does nothing",
			v:    Verdict{State: "stopped_short"},
			want: ActNone,
		},
		{
			name: "stopped short needing the user notifies, not nudges",
			v:    Verdict{State: "stopped_short", Callout: "x", NeedsUser: true},
			want: ActNotify,
		},
		{
			name: "blocked needing the user notifies",
			v:    Verdict{State: "blocked", NeedsUser: true},
			want: ActNotify,
		},
		{
			name: "blocked on its own does nothing",
			v:    Verdict{State: "blocked"},
			want: ActNone,
		},
		{
			name:    "working clears the stall memory",
			v:       Verdict{State: "working"},
			m:       SessionMemory{Nudges: 2, Notified: true},
			want:    ActNone,
			nudges:  0,
			cleared: true,
		},
		{
			name:    "done clears the stall memory",
			v:       Verdict{State: "done"},
			m:       SessionMemory{Nudges: 3, Notified: true},
			want:    ActNone,
			nudges:  0,
			cleared: true,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, m := Decide(c.v, c.m, maxNudges)
			if got != c.want {
				t.Errorf("action = %d, want %d", got, c.want)
			}
			if m.Nudges != c.nudges {
				t.Errorf("Nudges = %d, want %d", m.Nudges, c.nudges)
			}
			if c.cleared && m.Notified {
				t.Errorf("Notified = true, want cleared")
			}
		})
	}
}
