package main

import (
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/spf13/cobra"

	"github.com/tieo/proj/internal/config"
	"github.com/tieo/proj/internal/daemon"
	"github.com/tieo/proj/internal/handoff"
	"github.com/tieo/proj/internal/projects"
	"github.com/tieo/proj/internal/sessions"
	"github.com/tieo/proj/internal/tmux"
)

var switchDryRun bool

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
		fmt.Println(t.Prompt())
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
	if err := reg.SetTool(p.Name, to); err != nil {
		return err
	}

	session := projects.SessionName(p.Name, p.Tags)
	if live := tmux.SessionForPath(p.Dir); live != "" {
		if err := closeSession(live, false); err != nil {
			return err
		}
		// tmux tears the session down asynchronously; a beat keeps the
		// relaunch from colliding with the dying one over the session name.
		time.Sleep(500 * time.Millisecond)
	}

	if prompt != "" {
		// No native store to translate into: fresh launch with the transcript
		// as the initial prompt.
		cmdLine := daemon.PromptLaunchCommand(spec, p.Name, session, p.Dir, prompt)
		if _, err := tmux.NewSession(session, p.Dir, cmdLine); err != nil {
			return fmt.Errorf("create tmux session: %w", err)
		}
		fmt.Printf("switched %s to %s (handoff via initial prompt)\n", p.Name, to)
		if headless {
			return nil
		}
		return tmux.Attach(session)
	}

	// Native translation (or nothing to carry): the normal open path resumes
	// the freshly written session via the tool's own resume command.
	p.Tool = to
	fmt.Printf("switched %s to %s\n", p.Name, to)
	return openInTmux(cfg, p)
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
