package main

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/spf13/cobra"

	"github.com/tieo/proj/internal/config"
	"github.com/tieo/proj/internal/daemon"
	"github.com/tieo/proj/internal/handoff"
	"github.com/tieo/proj/internal/projects"
	"github.com/tieo/proj/internal/sessions"
	"github.com/tieo/proj/internal/tmux"
)

var switchDryRun bool

// keepHandoffs bounds the artifacts kept per project. Each one holds a full
// pre-cutoff history, and the tv artifact alone is 6 MB, so they cannot
// accumulate unbounded. Ten hops back is further than any handoff chain has
// been walked.
const keepHandoffs = 10

var switchCmd = &cobra.Command{
	Use:   "switch <project> <tool>",
	Short: "switch a project to another coding tool, carrying the conversation over",
	Long: `Switch a project's coding tool and carry the current conversation over.

The running session's conversation is read from the current tool's native
transcript into an intermediate transcript (user turns, assistant turns, tool
actions flattened to text; thinking and raw tool records don't survive any
translation). For claude and codex targets it is written as a native session
the new tool resumes as its own; agy's conversation store cannot be written,
so there the transcript rides in as an initial prompt. The running session is
closed and relaunched with the new tool.

Each switch saves the intermediate transcript under the proj state directory,
so a bad translation can be inspected. --dry-run prints it and changes
nothing.`,
	Args: cobra.ExactArgs(2),
	RunE: runSwitch,
}

func init() {
	switchCmd.Flags().BoolVar(&switchDryRun, "dry-run", false, "print the extracted transcript, switch nothing")
	rootCmd.AddCommand(switchCmd)
}

func runSwitch(cmd *cobra.Command, args []string) error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	p, err := projects.Resolve(cfg.BaseDir, args[0])
	if err != nil {
		return err
	}
	from := daemon.ToolName(p.Tool)
	to := daemon.ToolName(args[1])
	spec, err := cfg.Tool(to)
	if err != nil {
		return err
	}
	if from == to {
		return fmt.Errorf("%s already runs %s", p.Name, to)
	}

	t, err := extractTranscript(cfg, from, p.Dir)
	if err != nil {
		return fmt.Errorf("read %s transcript: %w", from, err)
	}
	if switchDryRun {
		fmt.Printf("%s -> %s: %d turns extracted; %d turns would be injected into %s\n", from, to, len(t.Turns), len(t.TargetTurns()), to)
		fmt.Println(t.PromptWithArtifact(""))
		return nil
	}

	artifactPath := ""
	if !t.Empty() {
		path, err := t.Save(handoffDir(), p.Name)
		if err != nil {
			return err
		}
		artifactPath = path
		fmt.Printf("extracted %d turns from %s (%s)\n", len(t.Turns), from, path)
		// A failed prune leaves extra artifacts behind, which costs disk and
		// nothing else, so it must not abort a switch that already wrote one.
		if err := handoff.Prune(handoffDir(), p.Name, keepHandoffs); err != nil {
			fmt.Fprintf(os.Stderr, "warning: prune handoff artifacts: %v\n", err)
		}
	}

	// Translate before touching anything live: a failed write leaves the
	// project on its old tool with its session intact.
	prompt := ""
	if !t.Empty() {
		switch to {
		case config.DefaultTool:
			id, err := handoff.WriteClaude(t, sessions.Home(cfg.Claude.Home), p.Dir, artifactPath)
			if err != nil {
				return fmt.Errorf("write claude session: %w", err)
			}
			fmt.Printf("translated into claude session %s\n", id)
		case "codex":
			id, err := handoff.WriteCodex(t, handoff.CodexHome(), p.Dir, artifactPath)
			if err != nil {
				return fmt.Errorf("write codex rollout: %w", err)
			}
			fmt.Printf("translated into codex session %s\n", id)
		default:
			prompt = t.PromptWithArtifact(artifactPath)
		}
	}

	reg, err := projects.LoadRegistry()
	if err != nil {
		return err
	}

	session := projects.SessionName(p.Name, p.Tags)
	live := tmux.SessionForPath(p.Dir)

	// With a prompt there is no native store to resume from, so the transcript
	// rides in as the initial prompt; otherwise the tool's own resume command
	// picks up the session that was just written for it.
	cmdLine := daemon.LaunchCommand(spec, cfg.Claude.Home, p.Name, session, p.Dir)
	handoffVia := ""
	if prompt != "" {
		cmdLine = daemon.PromptLaunchCommand(spec, p.Name, session, p.Dir, prompt)
		handoffVia = " (handoff via initial prompt)"
	}

	if live == "" {
		if _, err := tmux.NewSession(session, p.Dir, cmdLine); err != nil {
			return fmt.Errorf("create tmux session: %w", err)
		}
	} else {
		// Swap the tool inside the session rather than killing it: a kill
		// detaches every client watching the project, and the tags in the
		// session name may have drifted since it was created. Respawning keeps
		// the session and pane ids, so an attached terminal follows the project
		// across the switch.
		if live != session {
			if err := tmux.RenameSession(live, session); err != nil {
				return fmt.Errorf("rename session %q: %w", live, err)
			}
		}
		if err := tmux.RespawnSession(session, p.Dir, cmdLine); err != nil {
			return fmt.Errorf("respawn session %q: %w", session, err)
		}
	}

	// Record the new tool only once the pane actually runs it. Recording first
	// leaves the registry naming a tool the session never launched when the
	// swap fails, and the next switch then refuses as already on that tool.
	if err := reg.SetTool(p.Name, to); err != nil {
		return err
	}
	fmt.Printf("switched %s to %s%s\n", p.Name, to, handoffVia)
	if headless {
		return nil
	}
	return tmux.Attach(session)
}

func handoffDir() string {
	base := os.Getenv("XDG_STATE_HOME")
	if base == "" {
		home, _ := os.UserHomeDir()
		base = filepath.Join(home, ".local", "state")
	}
	return filepath.Join(base, "proj", "handoffs")
}

// extractTranscript reads the current tool's native transcript for dir into
// the intermediate representation. A tool with no history yields an empty
// transcript, which switches cleanly with nothing to carry.
func extractTranscript(cfg config.Config, from, dir string) (*handoff.Transcript, error) {
	switch from {
	case config.DefaultTool:
		path := daemon.RecentSessionFile(cfg.Claude.Home, dir)
		if path == "" {
			return &handoff.Transcript{SourceTool: from, Cwd: dir}, nil
		}
		return handoff.ReadClaude(path, dir)
	case "codex":
		path := handoff.RecentCodexRollout(dir)
		if path == "" {
			return &handoff.Transcript{SourceTool: from, Cwd: dir}, nil
		}
		return handoff.ReadCodex(path, dir)
	case "agy":
		return handoff.ReadAgy(handoff.AgyHistoryPath(), dir)
	default:
		return &handoff.Transcript{SourceTool: from, Cwd: dir}, nil
	}
}
