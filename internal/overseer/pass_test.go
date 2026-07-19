package overseer

import "testing"

func TestDecide(t *testing.T) {
	const maxNudges = 3
	cases := []struct {
		name    string
		v       Verdict
		sl      sessLook
		want    action
		nudges  int  // expected Nudges in the returned memory
		cleared bool // expected Notified == false in the returned memory
	}{
		{
			name: "stopped short with a callout nudges",
			v:    Verdict{State: "stopped_short", Callout: "keep going"},
			want: actNudge,
		},
		{
			name:   "stopped short at the nudge limit does nothing",
			v:      Verdict{State: "stopped_short", Callout: "keep going"},
			sl:     sessLook{Nudges: maxNudges},
			want:   actNone,
			nudges: maxNudges, // preserved, not reset
		},
		{
			name: "stopped short without a callout does nothing",
			v:    Verdict{State: "stopped_short"},
			want: actNone,
		},
		{
			name: "stopped short needing the user notifies, not nudges",
			v:    Verdict{State: "stopped_short", Callout: "x", NeedsUser: true},
			want: actNotify,
		},
		{
			name: "blocked needing the user notifies",
			v:    Verdict{State: "blocked", NeedsUser: true},
			want: actNotify,
		},
		{
			name: "blocked on its own does nothing",
			v:    Verdict{State: "blocked"},
			want: actNone,
		},
		{
			name:    "working clears the stall memory",
			v:       Verdict{State: "working"},
			sl:      sessLook{Nudges: 2, Notified: true},
			want:    actNone,
			nudges:  0,
			cleared: true,
		},
		{
			name:    "done clears the stall memory",
			v:       Verdict{State: "done"},
			sl:      sessLook{Nudges: 3, Notified: true},
			want:    actNone,
			nudges:  0,
			cleared: true,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, sl := decide(c.v, c.sl, maxNudges)
			if got != c.want {
				t.Errorf("action = %d, want %d", got, c.want)
			}
			if sl.Nudges != c.nudges {
				t.Errorf("Nudges = %d, want %d", sl.Nudges, c.nudges)
			}
			if c.cleared && sl.Notified {
				t.Errorf("Notified = true, want cleared")
			}
		})
	}
}
