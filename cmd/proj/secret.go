package main

import (
	"bufio"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/spf13/cobra"
)

// The manager's secrets are credentials it needs that are NOT reachable through
// its claude.ai MCP connectors (those use OAuth and need no token). They are
// stored AES-256-GCM encrypted in the manager repo (secrets.enc, safe to commit
// as ciphertext); the 32-byte key lives at secretKeyPath in XDG_CONFIG (chmod
// 600, never committed, backed up out of band by the user). This is a
// self-contained Go-stdlib store: sops/age would be the richer tool, but the
// corporate network blocks the Go proxy, so they cannot be installed on this
// host. The on-disk workflow is the same (encrypted file in the repo, key
// outside it), so a later switch to sops is a drop-in.

const secretsStore = "secrets.enc"

// secretKeyPath is the AES key file, outside the manager repo so ciphertext can
// be committed while the key never is.
func secretKeyPath() string {
	base := os.Getenv("XDG_CONFIG_HOME")
	if base == "" {
		home, _ := os.UserHomeDir()
		base = filepath.Join(home, ".config")
	}
	return filepath.Join(base, "proj", "secret.key")
}

var secretCmd = &cobra.Command{
	Use:   "secret",
	Short: "manage the manager's encrypted secrets (non-MCP credentials)",
	Long: `Store and read credentials the manager needs that are NOT available through
its claude.ai MCP connectors. Secrets are AES-256-GCM encrypted; the ciphertext
lives in the manager repo (` + "`secrets.enc`" + `, safe to commit), the key lives at
` + "`$XDG_CONFIG_HOME/proj/secret.key`" + ` (chmod 600, never committed - back it up).

  proj manager secret init          generate the encryption key
  proj manager secret set  <KEY>    read a value from stdin and store it
  proj manager secret get  <KEY>    print a value (for use in a command)
  proj manager secret list          list stored keys (never values)
  proj manager secret rm   <KEY>    remove a key`,
}

var (
	secretInitCmd = &cobra.Command{Use: "init", Args: cobra.NoArgs, Short: "generate the encryption key", RunE: runSecretInit}
	secretSetCmd  = &cobra.Command{Use: "set <KEY>", Args: cobra.ExactArgs(1), Short: "store a value read from stdin", RunE: runSecretSet}
	secretGetCmd  = &cobra.Command{Use: "get <KEY>", Args: cobra.ExactArgs(1), Short: "print a stored value", RunE: runSecretGet}
	secretListCmd = &cobra.Command{Use: "list", Args: cobra.NoArgs, Short: "list stored keys", RunE: runSecretList}
	secretRmCmd   = &cobra.Command{Use: "rm <KEY>", Args: cobra.ExactArgs(1), Short: "remove a key", RunE: runSecretRm}
)

func init() {
	secretCmd.AddCommand(secretInitCmd, secretSetCmd, secretGetCmd, secretListCmd, secretRmCmd)
	managerCmd.AddCommand(secretCmd)
}

func runSecretInit(cmd *cobra.Command, args []string) error {
	if err := scaffoldManager(managerDir()); err != nil {
		return err
	}
	kp := secretKeyPath()
	if _, err := os.Stat(kp); err == nil {
		fmt.Printf("key already present at %s\n", kp)
		return nil
	}
	if _, err := loadSecretKey(); err != nil { // generates + persists on first miss
		return err
	}
	fmt.Printf("generated encryption key at %s\n", kp)
	fmt.Println("back it up: without it the secrets are unrecoverable. Store one with `proj manager secret set <KEY>`.")
	return nil
}

func runSecretSet(cmd *cobra.Command, args []string) error {
	m, err := loadSecrets()
	if err != nil {
		return err
	}
	value, err := readSecretValue()
	if err != nil {
		return err
	}
	m[args[0]] = value
	if err := saveSecrets(m); err != nil {
		return err
	}
	fmt.Printf("stored %s\n", args[0])
	return nil
}

// readSecretValue takes a secret from stdin. In a terminal it reads until
// Enter with echo off - paste the value and press Enter, nothing to Ctrl-D and
// no piping. When stdin is redirected or piped it takes everything, so `proj
// manager secret set K < file` still works. Either way the value never appears
// in argv or shell history. Trailing newlines are trimmed.
func readSecretValue() (string, error) {
	if info, _ := os.Stdin.Stat(); info != nil && info.Mode()&os.ModeCharDevice != 0 {
		return readTerminalSecret()
	}
	raw, err := io.ReadAll(os.Stdin)
	if err != nil {
		return "", err
	}
	return strings.TrimRight(string(raw), "\r\n"), nil
}

// terminalPasteWindow is how long the reader waits for more of a paste after
// the first line ends. A paste lands in the tty buffer in one burst, so its
// remaining lines are already in flight and arrive well inside the window; a
// value typed by hand is followed by nothing, so the window simply elapses.
const terminalPasteWindow = 150 * time.Millisecond

// readTerminalSecret reads a secret from the terminal without echoing it, so
// the value never reaches the screen (nor a tmux pane's scrollback, which
// `proj` itself can capture). Multi-line values survive: a key pasted whole is
// taken whole rather than truncated at its first newline, silently storing a
// broken credential.
func readTerminalSecret() (string, error) {
	fmt.Fprint(os.Stderr, "paste the value, then press Enter (not echoed): ")
	restore := disableTerminalEcho()
	defer func() {
		restore()
		fmt.Fprintln(os.Stderr)
	}()

	lines := make(chan string)
	go func() {
		r := bufio.NewReader(os.Stdin)
		for {
			line, err := r.ReadString('\n')
			if line != "" {
				lines <- line
			}
			if err != nil {
				close(lines)
				return
			}
		}
	}()

	var b strings.Builder
	line, ok := <-lines
	if !ok {
		return "", nil
	}
	b.WriteString(line)
	for {
		select {
		case line, ok := <-lines:
			if !ok {
				return strings.TrimRight(b.String(), "\r\n"), nil
			}
			b.WriteString(line)
		case <-time.After(terminalPasteWindow):
			return strings.TrimRight(b.String(), "\r\n"), nil
		}
	}
}

// disableTerminalEcho stops the terminal from echoing what is typed and
// returns the call that turns echo back on. stty is used rather than a termios
// binding because proj depends on the Go standard library alone (the corporate
// network blocks the module proxy, see the store's header comment). Best
// effort: on a terminal that refuses the change the read still works, it just
// echoes.
func disableTerminalEcho() func() {
	stty := func(arg string) error {
		c := exec.Command("stty", arg)
		c.Stdin = os.Stdin
		return c.Run()
	}
	if err := stty("-echo"); err != nil {
		return func() {}
	}
	return func() { _ = stty("echo") }
}

func runSecretGet(cmd *cobra.Command, args []string) error {
	m, err := loadSecrets()
	if err != nil {
		return err
	}
	v, ok := m[args[0]]
	if !ok {
		return fmt.Errorf("%s not set", args[0])
	}
	fmt.Print(v) // no newline: safe to pipe/capture
	return nil
}

func runSecretList(cmd *cobra.Command, args []string) error {
	m, err := loadSecrets()
	if err != nil {
		return err
	}
	if len(m) == 0 {
		fmt.Println("(no secrets stored)")
		return nil
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		fmt.Println(k)
	}
	return nil
}

func runSecretRm(cmd *cobra.Command, args []string) error {
	m, err := loadSecrets()
	if err != nil {
		return err
	}
	if _, ok := m[args[0]]; !ok {
		return fmt.Errorf("%s not set", args[0])
	}
	delete(m, args[0])
	if err := saveSecrets(m); err != nil {
		return err
	}
	fmt.Printf("removed %s\n", args[0])
	return nil
}

// loadSecretKey returns the 32-byte AES key, generating and persisting it (chmod
// 600) on first use. A key file with the wrong size is a hard error, not a
// silent regenerate, so existing ciphertext is never orphaned unnoticed.
func loadSecretKey() ([]byte, error) {
	kp := secretKeyPath()
	if b, err := os.ReadFile(kp); err == nil {
		key, derr := base64.StdEncoding.DecodeString(strings.TrimSpace(string(b)))
		if derr != nil || len(key) != 32 {
			return nil, fmt.Errorf("secret key at %s is corrupt (expected 32 bytes base64)", kp)
		}
		return key, nil
	}
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Dir(kp), 0o700); err != nil {
		return nil, err
	}
	if err := os.WriteFile(kp, []byte(base64.StdEncoding.EncodeToString(key)), 0o600); err != nil {
		return nil, err
	}
	return key, nil
}

func secretsPath() string { return filepath.Join(managerDir(), secretsStore) }

// loadSecrets decrypts the store into a map. A missing store is an empty map.
func loadSecrets() (map[string]string, error) {
	key, err := loadSecretKey()
	if err != nil {
		return nil, err
	}
	raw, err := os.ReadFile(secretsPath())
	if os.IsNotExist(err) {
		return map[string]string{}, nil
	}
	if err != nil {
		return nil, err
	}
	blob, err := base64.StdEncoding.DecodeString(strings.TrimSpace(string(raw)))
	if err != nil {
		return nil, fmt.Errorf("secrets store is corrupt: %w", err)
	}
	gcm, err := newGCM(key)
	if err != nil {
		return nil, err
	}
	ns := gcm.NonceSize()
	if len(blob) < ns {
		return nil, fmt.Errorf("secrets store is truncated")
	}
	plain, err := gcm.Open(nil, blob[:ns], blob[ns:], nil)
	if err != nil {
		return nil, fmt.Errorf("decrypt failed (wrong key?): %w", err)
	}
	m := map[string]string{}
	if err := json.Unmarshal(plain, &m); err != nil {
		return nil, err
	}
	return m, nil
}

// saveSecrets encrypts the map and writes it atomically, so a crash mid-write
// cannot leave a half-written (unreadable) store.
func saveSecrets(m map[string]string) error {
	key, err := loadSecretKey()
	if err != nil {
		return err
	}
	plain, err := json.Marshal(m)
	if err != nil {
		return err
	}
	gcm, err := newGCM(key)
	if err != nil {
		return err
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return err
	}
	blob := gcm.Seal(nonce, nonce, plain, nil)
	enc := base64.StdEncoding.EncodeToString(blob)
	if err := scaffoldManager(managerDir()); err != nil {
		return err
	}
	tmp := secretsPath() + ".tmp"
	if err := os.WriteFile(tmp, []byte(enc+"\n"), 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, secretsPath())
}

func newGCM(key []byte) (cipher.AEAD, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}
