// Package handoff translates a conversation between the coding tools' native
// transcript formats through one intermediate representation, so a project can
// switch tools mid-task without losing context.
//
// The IR deliberately carries only what survives translation in every
// direction: user text, assistant text, and tool activity flattened to a
// one-line description. Thinking/reasoning blocks are cryptographically opaque
// in both claude and codex transcripts, and tool-call records are validated
// against provider-specific tool namespaces (Bash/Read/Edit vs
// shell/apply_patch), so neither can move between tools.
//
// Readers parse a tool's native store into the IR; writers emit the IR as a
// native session the target tool resumes as its own (claude jsonl, codex
// rollout), or as a prompt for tools whose store cannot be written (agy keeps
// conversations as undocumented protobuf blobs in sqlite).
package handoff

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// Turn is one IR conversation entry.
type Turn struct {
	Role string `json:"role"`           // "user", "assistant", or "tool"
	Name string `json:"name,omitempty"` // tool name when Role is "tool"
	Text string `json:"text"`
}

// Transcript is the intermediate representation of a conversation.
type Transcript struct {
	Version     int    `json:"version"`
	SourceTool  string `json:"source_tool"`
	Cwd         string `json:"cwd"`
	ExtractedAt string `json:"extracted_at"`
	Turns       []Turn `json:"turns"`
}

// Caps applied by every reader/writer. The saved IR keeps every extracted turn
// for audit/recovery, while target tool histories are bounded by total size so
// they do not overwhelm the next tool. Size is the only bound: a turn count
// spends the budget on whichever turns are most numerous, and tool calls
// outnumber user messages by roughly seven to one.
const (
	maxTurnText        = 4000
	maxTranscriptChars = 240000
)

func capText(s string) string {
	s = strings.TrimSpace(s)
	if len(s) > maxTurnText {
		return s[:maxTurnText] + " …[truncated]"
	}
	return s
}

// capTurns fits the transcript into maxTranscriptChars by evicting the oldest
// turns first, tool calls before assistant replies. User turns carry the intent
// the next tool has to continue, and cost a few percent of the budget, so they
// are never evicted; a session whose user turns alone exceed the budget keeps
// them and overruns. Surviving turns stay in order, with the gaps reported as
// omitted rather than silently closed.
func capTurns(turns []Turn) []Turn {
	total := 0
	for _, turn := range turns {
		total += len(turn.Text)
	}
	if total <= maxTranscriptChars {
		return turns
	}
	dropped := make([]bool, len(turns))
	for _, role := range []string{"tool", "assistant"} {
		for i := 0; i < len(turns) && total > maxTranscriptChars; i++ {
			if dropped[i] || turns[i].Role != role {
				continue
			}
			dropped[i] = true
			total -= len(turns[i].Text)
		}
	}
	kept := make([]Turn, 0, len(turns))
	for i, turn := range turns {
		if !dropped[i] {
			kept = append(kept, turn)
		}
	}
	return kept
}

func (t *Transcript) targetTurns() []Turn {
	if t == nil {
		return nil
	}
	return capTurns(stripHandoffNotes(t.Turns))
}

// notePrefix opens the note a writer prepends to a native target history. The
// next extraction reads that note back as a user turn, so without stripping,
// one note per switch survives forever: they are user turns, which capTurns
// never evicts.
const notePrefix = "[Handoff:"

// stripHandoffNotes drops the notes left by earlier switches. Nothing is lost:
// the note names the artifact of its own hop, and that artifact holds the note
// of the hop before it, so the chain stays walkable from the newest artifact
// backwards. The saved IR keeps every note for audit.
func stripHandoffNotes(turns []Turn) []Turn {
	kept := make([]Turn, 0, len(turns))
	for _, turn := range turns {
		if turn.Role == "user" && strings.HasPrefix(strings.TrimSpace(turn.Text), notePrefix) {
			continue
		}
		kept = append(kept, turn)
	}
	return kept
}

// TargetTurns returns the bounded recent tail injected into target tools.
func (t *Transcript) TargetTurns() []Turn {
	return t.targetTurns()
}

// omittedTurns counts conversation dropped by the size bound. Notes from
// earlier switches are not conversation, so stripping them is not an omission.
func (t *Transcript) omittedTurns() int {
	if t == nil {
		return 0
	}
	return len(stripHandoffNotes(t.Turns)) - len(t.targetTurns())
}

// Empty reports whether the transcript carries no conversation.
func (t *Transcript) Empty() bool {
	return t == nil || len(t.Turns) == 0
}

// Save writes the IR as JSON to dir/<project>-<unix>.json and returns the
// path. Every switch dumps its IR there, so a bad translation can be debugged
// from the artifact instead of reconstructed.
func (t *Transcript) Save(dir, project string) (string, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	path := filepath.Join(dir, fmt.Sprintf("%s-%d.json", project, time.Now().Unix()))
	data, err := json.MarshalIndent(t, "", "  ")
	if err != nil {
		return "", err
	}
	return path, os.WriteFile(path, data, 0o644)
}

// Prune keeps the newest keep artifacts for project and removes the rest.
// Each note names its own artifact, so pruning a hop truncates how far back
// the chain can be walked; keep is the number of switches whose full history
// stays recoverable. Names embed a unix stamp, so lexical order is age order
// until the stamp gains a digit, which lands in 2286.
func Prune(dir, project string, keep int) error {
	if keep < 1 {
		return fmt.Errorf("keep must be at least 1, got %d", keep)
	}
	matches, err := filepath.Glob(filepath.Join(dir, project+"-*.json"))
	if err != nil {
		return err
	}
	if len(matches) <= keep {
		return nil
	}
	sort.Strings(matches)
	for _, path := range matches[:len(matches)-keep] {
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			return err
		}
	}
	return nil
}

// HandoffNote returns the short message inserted into native target histories.
func (t *Transcript) HandoffNote(artifactPath string) string {
	note := fmt.Sprintf("Handoff: this conversation was translated from %s by proj switch on %s. Tool actions appear as bracketed text.", t.SourceTool, t.ExtractedAt)
	if omitted := t.omittedTurns(); omitted > 0 {
		note += fmt.Sprintf(" This is a bounded recent-history cutoff: %d older extracted turns were omitted from the target history.", omitted)
		if artifactPath == "" {
			note += " No handoff JSON was written, so they cannot be recovered."
		}
	}
	if artifactPath != "" {
		note += fmt.Sprintf(" Read the omitted turns in the full extracted handoff JSON: %s", artifactPath)
	}
	return "[" + note + "]"
}

// PromptWithArtifact renders the IR as a handoff message for tools whose
// native store cannot be written. The framing tells the model it is taking
// over, since unlike a native-format resume it cannot infer that from its own
// history. artifactPath points at the saved full IR; empty means none was
// written, which a cutoff notice has to say outright, or the model is told
// that turns are missing with no way to reach them.
func (t *Transcript) PromptWithArtifact(artifactPath string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "You are taking over an ongoing coding session in this directory from %s (the user switched tools). Recent conversation:\n\n", t.SourceTool)
	if omitted := t.omittedTurns(); omitted > 0 {
		fmt.Fprintf(&b, "Note: this prompt contains a bounded recent-history cutoff. %d older extracted turns are omitted here.", omitted)
		if artifactPath != "" {
			fmt.Fprintf(&b, " Read them in the full extracted handoff JSON: %s", artifactPath)
		} else {
			b.WriteString(" No handoff JSON was written, so they cannot be recovered.")
		}
		b.WriteString("\n\n")
	} else if artifactPath != "" {
		fmt.Fprintf(&b, "Full extracted handoff JSON: %s\n\n", artifactPath)
	}
	for _, turn := range t.targetTurns() {
		switch turn.Role {
		case "user":
			fmt.Fprintf(&b, "[User] %s\n", turn.Text)
		case "assistant":
			fmt.Fprintf(&b, "[%s] %s\n", t.SourceTool, turn.Text)
		case "tool":
			fmt.Fprintf(&b, "[%s ran %s] %s\n", t.SourceTool, turn.Name, turn.Text)
		}
	}
	b.WriteString("\nContinue the work where it left off. Inspect the repository state (git status, recent commits) before changing anything if the next step is unclear.")
	return b.String()
}

// compactJSON flattens a raw JSON value to a single short line for tool turns.
func compactJSON(raw json.RawMessage) string {
	var v any
	if json.Unmarshal(raw, &v) != nil {
		return ""
	}
	out, err := json.Marshal(v)
	if err != nil {
		return ""
	}
	s := string(out)
	if len(s) > 200 {
		s = s[:200] + "…"
	}
	return s
}
