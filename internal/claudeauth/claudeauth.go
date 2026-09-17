// Package claudeauth keeps named Claude Code logins and switches between them.
//
// A Claude Code login is two pieces of state and nothing else: the OAuth tokens
// in <root>/.credentials.json and the oauthAccount block (email, organization,
// plan) in the .claude.json next to the root. Transcripts, memory, settings,
// hooks and Remote Control files do not depend on the account, so switching is
// swapping that pair while every other file stays where Claude Code expects it,
// and resumed conversations carry across the switch.
package claudeauth

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

const (
	credentialsFile = "credentials.json"
	accountFile     = "account.json"
)

// Account is a saved login as the picker shows it.
type Account struct {
	Name  string
	Email string
	Org   string
	Plan  Plan
	OrgID string
	key   string
}

// Plan is what an account is entitled to: the subscription it runs on, the seat
// an organization gave it, the rate limit tier the tokens carry, and whether
// usage past the limit is allowed. The tier lives in the tokens file and the
// rest in the account details, so a plan is only complete with both.
type Plan struct {
	Subscription string // "max", "team", "pro"
	Seat         string // organization seat, e.g. "team_tier_1"
	Tier         string // rate limit tier, e.g. "default_claude_max_5x"
	ExtraUsage   bool   // usage past the limit allowed
	OrgType      string // "claude_team" for an organization, empty for a personal one
}

// String renders a plan the way the lists show it: the subscription, the rate
// limit tier in the short form people use for it ("5x"), and the limits that
// differ from the plain plan.
func (p Plan) String() string {
	out := p.Subscription
	if out == "" {
		out = "unknown plan"
	}
	if t := shortTier(p.Tier); t != "" {
		out += " " + t
	}
	if !p.ExtraUsage {
		out += ", no extra usage"
	}
	return out
}

// shortTier turns "default_claude_max_5x" into "5x". The tier names carry a
// "default_" prefix and the product name, and only the multiplier at the end
// tells two seats apart.
func shortTier(tier string) string {
	t := strings.TrimPrefix(tier, "default_")
	t = strings.TrimPrefix(t, "claude_")
	if i := strings.LastIndex(t, "_"); i >= 0 {
		if tail := t[i+1:]; strings.HasSuffix(tail, "x") {
			return tail
		}
	}
	return ""
}

// Identity is the account a login belongs to. The organization is part of it:
// one person can hold a personal plan and a seat in an employer's organization
// under the same user id, and those are the two logins worth switching between.
type Identity struct {
	AccountUUID      string `json:"accountUuid"`
	OrganizationUUID string `json:"organizationUuid"`
	EmailAddress     string `json:"emailAddress"`
	OrganizationName string `json:"organizationName"`
	OrganizationType string `json:"organizationType"`
	SeatTier         string `json:"seatTier"`
	ExtraUsage       bool   `json:"hasExtraUsageEnabled"`
}

func (i Identity) key() string { return i.AccountUUID + "/" + i.OrganizationUUID }

// Store is the directory holding one subdirectory per saved account.
type Store struct {
	Dir string
}

// Live is the login Claude Code is using now, read from its root.
type Live struct {
	Root        string
	Credentials []byte
	Account     json.RawMessage
	Identity    Identity
	Plan        Plan
}

var nameRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)

// ValidName reports whether name can be used as an account name, which is also
// a directory name.
func ValidName(name string) error {
	if !nameRE.MatchString(name) {
		return fmt.Errorf("account name %q: use letters, digits, '.', '_' or '-', starting with a letter or digit", name)
	}
	return nil
}

// ConfigPath is the .claude.json that holds the oauthAccount block for root.
func ConfigPath(root string) string {
	return filepath.Join(filepath.Dir(root), ".claude.json")
}

// ReadLive reads the login Claude Code under root is using.
func ReadLive(root string) (Live, error) {
	creds, err := os.ReadFile(filepath.Join(root, ".credentials.json"))
	if err != nil {
		return Live{}, fmt.Errorf("read the current login's tokens: %w", err)
	}
	cfg, err := readConfig(ConfigPath(root))
	if err != nil {
		return Live{}, err
	}
	raw, ok := cfg["oauthAccount"]
	if !ok {
		return Live{}, fmt.Errorf("%s has no oauthAccount; log in with /login first", ConfigPath(root))
	}
	account, err := json.Marshal(raw)
	if err != nil {
		return Live{}, fmt.Errorf("encode oauthAccount from %s: %w", ConfigPath(root), err)
	}
	var id Identity
	if err := json.Unmarshal(account, &id); err != nil {
		return Live{}, fmt.Errorf("parse oauthAccount in %s: %w", ConfigPath(root), err)
	}
	return Live{Root: root, Credentials: creds, Account: account, Identity: id, Plan: planOf(creds, id)}, nil
}

// planOf assembles the plan from the tokens (subscription and rate limit tier)
// and the account details (seat, extra usage), for display only.
func planOf(creds []byte, id Identity) Plan {
	var c struct {
		ClaudeAiOauth struct {
			SubscriptionType string `json:"subscriptionType"`
			RateLimitTier    string `json:"rateLimitTier"`
		} `json:"claudeAiOauth"`
	}
	_ = json.Unmarshal(creds, &c)
	return Plan{
		Subscription: c.ClaudeAiOauth.SubscriptionType,
		Tier:         c.ClaudeAiOauth.RateLimitTier,
		Seat:         id.SeatTier,
		ExtraUsage:   id.ExtraUsage,
		OrgType:      id.OrganizationType,
	}
}

// List returns the saved accounts sorted by name.
func (s Store) List() ([]Account, error) {
	entries, err := os.ReadDir(s.Dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("list saved accounts in %s: %w", s.Dir, err)
	}
	var out []Account
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		a, err := s.load(e.Name())
		if err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

func (s Store) load(name string) (Account, error) {
	dir := filepath.Join(s.Dir, name)
	account, err := os.ReadFile(filepath.Join(dir, accountFile))
	if err != nil {
		return Account{}, fmt.Errorf("read saved account %q: %w", name, err)
	}
	creds, err := os.ReadFile(filepath.Join(dir, credentialsFile))
	if err != nil {
		return Account{}, fmt.Errorf("read saved account %q: %w", name, err)
	}
	var id Identity
	if err := json.Unmarshal(account, &id); err != nil {
		return Account{}, fmt.Errorf("parse saved account %q: %w", name, err)
	}
	return Account{
		Name:  name,
		Email: id.EmailAddress,
		Org:   id.OrganizationName,
		Plan:  planOf(creds, id),
		OrgID: id.OrganizationUUID,
		key:   id.key(),
	}, nil
}

// Find returns the saved account holding the same login as live, if any.
func (s Store) Find(live Live) (Account, bool, error) {
	accounts, err := s.List()
	if err != nil {
		return Account{}, false, err
	}
	for _, a := range accounts {
		if a.key == live.Identity.key() {
			return a, true, nil
		}
	}
	return Account{}, false, nil
}

// Save stores live under name, replacing what name held. Saving a login that is
// already stored under another name is refused, since the two copies would
// drift apart as tokens rotate.
func (s Store) Save(name string, live Live) error {
	if err := ValidName(name); err != nil {
		return err
	}
	if existing, ok, err := s.Find(live); err != nil {
		return err
	} else if ok && existing.Name != name {
		return fmt.Errorf("%s is already saved as %q", live.Identity.EmailAddress, existing.Name)
	}
	dir := filepath.Join(s.Dir, name)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create %s: %w", dir, err)
	}
	if err := writeAtomic(filepath.Join(dir, credentialsFile), live.Credentials, 0o600); err != nil {
		return fmt.Errorf("save tokens for %q: %w", name, err)
	}
	if err := writeAtomic(filepath.Join(dir, accountFile), live.Account, 0o600); err != nil {
		return fmt.Errorf("save account details for %q: %w", name, err)
	}
	return nil
}

// Refresh copies the live tokens into the saved account holding the same login.
// Claude Code rotates tokens as it refreshes them, so a copy taken once goes
// stale; refreshing before every switch keeps the saved one usable. It returns
// the name refreshed, empty when the live login is not saved.
func (s Store) Refresh(live Live) (string, error) {
	a, ok, err := s.Find(live)
	if err != nil || !ok {
		return "", err
	}
	return a.Name, s.Save(a.Name, live)
}

// Remove deletes a saved account. The live login is not touched.
func (s Store) Remove(name string) error {
	if err := ValidName(name); err != nil {
		return err
	}
	dir := filepath.Join(s.Dir, name)
	if _, err := os.Stat(dir); err != nil {
		return fmt.Errorf("no saved account %q: %w", name, err)
	}
	return os.RemoveAll(dir)
}

// Apply makes name the login Claude Code under root uses: its tokens replace
// .credentials.json and its account block replaces oauthAccount in .claude.json,
// leaving every other key of that file as it was. A Claude Code process still
// running keeps the old login in memory and writes it back when it refreshes,
// so callers stop those processes first.
func (s Store) Apply(name, root string) error {
	dir := filepath.Join(s.Dir, name)
	creds, err := os.ReadFile(filepath.Join(dir, credentialsFile))
	if err != nil {
		return fmt.Errorf("read saved account %q: %w", name, err)
	}
	account, err := os.ReadFile(filepath.Join(dir, accountFile))
	if err != nil {
		return fmt.Errorf("read saved account %q: %w", name, err)
	}
	path := ConfigPath(root)
	cfg, err := readConfig(path)
	if err != nil {
		return err
	}
	dec := json.NewDecoder(bytes.NewReader(account))
	dec.UseNumber()
	var block any
	if err := dec.Decode(&block); err != nil {
		return fmt.Errorf("parse saved account %q: %w", name, err)
	}
	cfg["oauthAccount"] = block
	out, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return fmt.Errorf("encode %s: %w", path, err)
	}
	// Tokens first: if the second write fails, the tokens already belong to the
	// new account and Claude Code refetches the profile from them, while the
	// reverse order would leave a profile describing an account with no tokens.
	if err := writeAtomic(filepath.Join(root, ".credentials.json"), creds, 0o600); err != nil {
		return fmt.Errorf("write tokens for %q to %s: %w", name, root, err)
	}
	if err := writeAtomic(path, out, 0o644); err != nil {
		return fmt.Errorf("tokens for %q are in place, but writing its account details to %s failed: %w", name, path, err)
	}
	return nil
}

func readConfig(path string) (map[string]any, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var cfg map[string]any
	if err := dec.Decode(&cfg); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	return cfg, nil
}

// writeAtomic replaces path in one step, so a Claude Code process starting up
// never reads a half-written file.
func writeAtomic(path string, data []byte, mode os.FileMode) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Chmod(mode); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}
