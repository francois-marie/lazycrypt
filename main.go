// Package main implements lazycrypt, a terminal UI for managing encrypted git
// histories. It maintains two parallel git repos: a plaintext local repo and an
// encrypted mirror where every file is encrypted with age. The encrypted mirror
// can be pushed to untrusted remotes like GitHub.
package main

import (
	"bufio"
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/charmbracelet/bubbles/key"
	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"gopkg.in/yaml.v3"
)

// keyFilePerms is the permission mode for age private key files.
const keyFilePerms = 0600

// --- Thread-safe command log ---

var (
	globalCmdLog   []string
	globalCmdLogMu sync.Mutex
)

func logCmd(format string, args ...interface{}) {
	globalCmdLogMu.Lock()
	defer globalCmdLogMu.Unlock()
	entry := fmt.Sprintf(format, args...)
	globalCmdLog = append(globalCmdLog, entry)
	if len(globalCmdLog) > 200 {
		globalCmdLog = globalCmdLog[len(globalCmdLog)-200:]
	}
}

func snapshotCmdLog() []string {
	globalCmdLogMu.Lock()
	defer globalCmdLogMu.Unlock()
	result := make([]string, len(globalCmdLog))
	copy(result, globalCmdLog)
	return result
}

// loggedCommand wraps exec.Command and logs the invocation to the global
// command log for display in the TUI.
func loggedCommand(name string, args ...string) *exec.Cmd {
	logCmd("$ %s %s", name, strings.Join(args, " "))
	return exec.Command(name, args...)
}

// --- Data types ---

// Config represents the .lazycrypt/config.yml file.
type Config struct {
	Version         int            `yaml:"version"`
	CurrentKey      string         `yaml:"current_key"`
	EncryptedRemote RemoteConfig   `yaml:"encrypted_remote"`
	ExcludePatterns []string       `yaml:"exclude_patterns"`
	RetiredKeys     []RetiredKey   `yaml:"retired_keys"`
}

// RemoteConfig holds the name and URL of the encrypted git remote.
type RemoteConfig struct {
	Name string `yaml:"name"`
	URL  string `yaml:"url"`
}

// RetiredKey records a previously active key that has been rotated out.
type RetiredKey struct {
	Path      string `yaml:"path"`
	RetiredAt string `yaml:"retired_at"`
}

// Commit represents a single git commit with its sync status.
type Commit struct {
	SHA      string
	ShortSHA string
	Message  string
	Synced   bool
}

// FileChange represents a file modification within a commit.
type FileChange struct {
	Status string // A, M, D
	Path   string
}

// Key represents an age encryption key (active or retired).
type Key struct {
	Path      string
	PublicKey string
	Active    bool
	RetiredAt string
}

// Remote represents a git remote with its type classification.
type Remote struct {
	Name     string
	URL      string
	RepoType string // "plaintext" or "encrypted"
}

// PrereqStatus captures the environment checks needed before lazycrypt can run.
type PrereqStatus struct {
	InsideRepo    bool
	RepoPath      string
	CurrentBranch string
	HasAge        bool
	HasGit        bool
	Initialized   bool
}

// --- Commit map ---

// CommitMap maintains the mapping between plaintext and encrypted commit SHAs.
// It is persisted as a simple "plaintext-sha:encrypted-sha" text file.
type CommitMap struct {
	mapping  map[string]string
	filePath string
}

func loadCommitMap(path string) *CommitMap {
	cm := &CommitMap{mapping: make(map[string]string), filePath: path}
	f, err := os.Open(path)
	if err != nil {
		return cm
	}
	defer f.Close()
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		parts := strings.SplitN(line, ":", 2)
		if len(parts) == 2 {
			cm.mapping[parts[0]] = parts[1]
		}
	}
	return cm
}

func (cm *CommitMap) save() error {
	var lines []string
	for k, v := range cm.mapping {
		lines = append(lines, k+":"+v)
	}
	return os.WriteFile(cm.filePath, []byte(strings.Join(lines, "\n")+"\n"), 0644)
}

func (cm *CommitMap) isSynced(sha string) bool {
	_, ok := cm.mapping[sha]
	return ok
}

func (cm *CommitMap) add(plaintextSHA, encryptedSHA string) {
	cm.mapping[plaintextSHA] = encryptedSHA
}

func (cm *CommitMap) syncedCount() int {
	return len(cm.mapping)
}

// --- Path helpers ---

func lazycryptDir() string      { return ".lazycrypt" }
func configPath() string        { return filepath.Join(lazycryptDir(), "config.yml") }
func commitMapPath() string     { return filepath.Join(lazycryptDir(), "commit-map") }
func encryptedRepoPath() string { return filepath.Join(lazycryptDir(), "encrypted.git") }
func keysDir() string           { return filepath.Join(lazycryptDir(), "keys") }
func currentKeyPath() string    { return filepath.Join(keysDir(), "current.key") }
func recvCommitMapPath() string { return filepath.Join(lazycryptDir(), "recv-commit-map") }

// --- Config loading ---

func loadConfig() (Config, error) {
	data, err := os.ReadFile(configPath())
	if err != nil {
		return Config{Version: 1}, nil
	}
	var cfg Config
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return Config{}, fmt.Errorf("parse config: %w", err)
	}
	return cfg, nil
}

func saveConfig(cfg Config) error {
	data, err := yaml.Marshal(cfg)
	if err != nil {
		return fmt.Errorf("marshal config: %w", err)
	}
	return os.WriteFile(configPath(), data, 0644)
}

// --- Prerequisites ---

func checkPrereqs() PrereqStatus {
	status := PrereqStatus{}

	if err := exec.Command("git", "rev-parse", "--is-inside-work-tree").Run(); err == nil {
		status.InsideRepo = true
		if out, err := exec.Command("git", "rev-parse", "--show-toplevel").Output(); err == nil {
			status.RepoPath = strings.TrimSpace(string(out))
		}
		if out, err := exec.Command("git", "branch", "--show-current").Output(); err == nil {
			status.CurrentBranch = strings.TrimSpace(string(out))
		}
	}

	if _, err := exec.LookPath("age"); err == nil {
		status.HasAge = true
	}
	if _, err := exec.LookPath("git"); err == nil {
		status.HasGit = true
	}
	if _, err := os.Stat(configPath()); err == nil {
		status.Initialized = true
	}

	return status
}

// --- Git helpers ---

// parseCommitLines parses "SHA message" lines into Commit slices.
// If cm is non-nil, each commit's Synced field is set from the map.
func parseCommitLines(output string, cm *CommitMap) []Commit {
	var commits []Commit
	for _, line := range strings.Split(strings.TrimSpace(output), "\n") {
		if line == "" {
			continue
		}
		parts := strings.SplitN(line, " ", 2)
		sha := parts[0]
		msg := ""
		if len(parts) > 1 {
			msg = parts[1]
		}
		synced := false
		if cm != nil {
			synced = cm.isSynced(sha)
		}
		shortSHA := sha
		if len(sha) >= 7 {
			shortSHA = sha[:7]
		}
		commits = append(commits, Commit{
			SHA:      sha,
			ShortSHA: shortSHA,
			Message:  msg,
			Synced:   synced,
		})
	}
	return commits
}

func getPlaintextCommits() []Commit {
	out, err := loggedCommand("git", "log", "--format=%H %s", "--reverse").Output()
	if err != nil {
		return nil
	}
	cm := loadCommitMap(commitMapPath())
	commits := parseCommitLines(string(out), cm)
	reverseCommits(commits)
	return commits
}

func getEncryptedCommits() []Commit {
	repoPath := encryptedRepoPath()
	if _, err := os.Stat(repoPath); err != nil {
		return nil
	}
	out, err := loggedCommand("git", "-C", repoPath, "log", "--format=%H %s").Output()
	if err != nil {
		return nil
	}
	return parseCommitLines(string(out), nil)
}

func reverseCommits(commits []Commit) {
	for i, j := 0, len(commits)-1; i < j; i, j = i+1, j-1 {
		commits[i], commits[j] = commits[j], commits[i]
	}
}

func getFilesForCommit(sha string) []FileChange {
	out, err := loggedCommand("git", "diff-tree", "--no-commit-id", "-r", "--name-status", "-z", sha).Output()
	if err != nil || len(out) == 0 {
		out, err = loggedCommand("git", "diff-tree", "--root", "--no-commit-id", "-r", "--name-status", "-z", sha).Output()
		if err != nil {
			return nil
		}
	}
	return parseNameStatusZ(out)
}

func parseFileChanges(output string) []FileChange {
	var files []FileChange
	for _, line := range strings.Split(strings.TrimSpace(output), "\n") {
		if line == "" {
			continue
		}
		parts := strings.Fields(line)
		if len(parts) >= 2 {
			files = append(files, FileChange{Status: parts[0], Path: parts[1]})
		}
	}
	return files
}

// parseNameStatusZ parses "git diff-tree --name-status -z" output into
// FileChange entries. The output is a NUL-separated stream of tokens:
//
//	status NUL path NUL [path2 NUL] status NUL path NUL ...
//
// For renames/copies (Rn or Cn) there is an extra path token; in that case we
// treat the second path as the new path.
func parseNameStatusZ(output []byte) []FileChange {
	if len(output) == 0 {
		return nil
	}
	var files []FileChange
	tokens := bytes.Split(output, []byte{0})
	// Drop possible trailing empty token from final NUL.
	if len(tokens) > 0 && len(tokens[len(tokens)-1]) == 0 {
		tokens = tokens[:len(tokens)-1]
	}
	for i := 0; i < len(tokens); {
		status := string(tokens[i])
		i++
		if status == "" {
			continue
		}
		if i >= len(tokens) {
			break
		}
		path := string(tokens[i])
		i++
		// Renames / copies have an extra "old-path" token; use the new path.
		if len(status) > 0 && (status[0] == 'R' || status[0] == 'C') {
			if i >= len(tokens) {
				break
			}
			path = string(tokens[i])
			i++
		}
		if path == "" {
			continue
		}
		files = append(files, FileChange{Status: status, Path: path})
	}
	return files
}

// matchesExcludePattern returns true if filePath matches any of the given glob
// patterns. Each pattern is checked against both the full path and the basename.
func matchesExcludePattern(filePath string, patterns []string) bool {
	base := filepath.Base(filePath)
	for _, pattern := range patterns {
		if matched, _ := filepath.Match(pattern, base); matched {
			return true
		}
		if matched, _ := filepath.Match(pattern, filePath); matched {
			return true
		}
	}
	return false
}

func getCommitDiff(sha string) string {
	out, err := loggedCommand("git", "show", "--stat", "--patch", sha).Output()
	if err != nil {
		return ""
	}
	return string(out)
}

func getCommitMeta(sha string) string {
	out, err := loggedCommand("git", "log", "-1", "--format=commit %H%nAuthor: %an <%ae>%nDate:   %ai%n%n    %s%n", sha).Output()
	if err != nil {
		return ""
	}
	return string(out)
}

func getRemotes() []Remote {
	var remotes []Remote
	out, err := loggedCommand("git", "remote").Output()
	if err != nil {
		return remotes
	}
	for _, name := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if name == "" {
			continue
		}
		urlOut, err := loggedCommand("git", "remote", "get-url", name).Output()
		if err != nil {
			continue
		}
		remotes = append(remotes, Remote{
			Name:     name,
			URL:      strings.TrimSpace(string(urlOut)),
			RepoType: "plaintext",
		})
	}
	cfg, _ := loadConfig()
	if cfg.EncryptedRemote.URL != "" {
		remotes = append(remotes, Remote{
			Name:     cfg.EncryptedRemote.Name,
			URL:      cfg.EncryptedRemote.URL,
			RepoType: "encrypted",
		})
	}
	return remotes
}

// getPublicKey extracts the age public key from a private key file.
// Age key files contain a comment line: "# public key: age1..."
func getPublicKey(keyPath string) string {
	data, err := os.ReadFile(keyPath)
	if err != nil {
		return ""
	}
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "# public key: ") {
			return strings.TrimPrefix(line, "# public key: ")
		}
	}
	return ""
}

func getKeys() []Key {
	var keys []Key
	if _, err := os.Stat(currentKeyPath()); err == nil {
		keys = append(keys, Key{
			Path:      currentKeyPath(),
			PublicKey: getPublicKey(currentKeyPath()),
			Active:    true,
		})
	}
	cfg, _ := loadConfig()
	for _, rk := range cfg.RetiredKeys {
		keys = append(keys, Key{
			Path:      rk.Path,
			PublicKey: getPublicKey(rk.Path),
			Active:    false,
			RetiredAt: rk.RetiredAt,
		})
	}
	return keys
}

// defaultEncryptedRemoteURL tries to derive a default encrypted remote URL
// from the plaintext origin remote. It appends "-lazycrypted" to the repo name.
// Returns "" if it cannot determine a sensible default.
func defaultEncryptedRemoteURL() string {
	out, err := exec.Command("git", "remote", "get-url", "origin").Output()
	if err != nil {
		return ""
	}
	originURL := strings.TrimSpace(string(out))
	if originURL == "" {
		return ""
	}
	// SSH format: git@host:user/repo.git
	if strings.Contains(originURL, ":") && strings.Contains(originURL, "@") {
		suffix := ".git"
		base := originURL
		if strings.HasSuffix(base, ".git") {
			base = strings.TrimSuffix(base, ".git")
		}
		return base + "-lazycrypted" + suffix
	}
	// HTTPS format: https://host/user/repo.git
	if strings.HasPrefix(originURL, "https://") || strings.HasPrefix(originURL, "http://") {
		suffix := ".git"
		base := originURL
		if strings.HasSuffix(base, ".git") {
			base = strings.TrimSuffix(base, ".git")
		}
		return base + "-lazycrypted" + suffix
	}
	return ""
}

// --- Age encryption/decryption ---

func ageEncrypt(plaintext []byte, publicKey string) ([]byte, error) {
	cmd := loggedCommand("age", "-r", publicKey)
	cmd.Stdin = bytes.NewReader(plaintext)
	out, err := cmd.Output()
	if err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			return nil, fmt.Errorf("age encrypt: %s", string(exitErr.Stderr))
		}
		return nil, err
	}
	return out, nil
}

func ageDecrypt(ciphertext []byte, keyPath string) ([]byte, error) {
	cmd := loggedCommand("age", "-d", "-i", keyPath)
	cmd.Stdin = bytes.NewReader(ciphertext)
	out, err := cmd.Output()
	if err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			return nil, fmt.Errorf("age decrypt: %s", string(exitErr.Stderr))
		}
		return nil, err
	}
	return out, nil
}

// --- Styles (lazygit-inspired terminal colors) ---

var (
	colorGreen  = lipgloss.Color("2")
	colorYellow = lipgloss.Color("3")
	colorRed    = lipgloss.Color("1")
	colorCyan   = lipgloss.Color("6")
	colorBlue   = lipgloss.Color("4")
	colorWhite  = lipgloss.Color("7")
	colorGray   = lipgloss.Color("8")

	boldStyle     = lipgloss.NewStyle().Bold(true)
	faintStyle    = lipgloss.NewStyle().Foreground(colorGray)
	selectedStyle = lipgloss.NewStyle().Bold(true).Background(colorCyan).Foreground(lipgloss.Color("0"))
	shaStyle      = lipgloss.NewStyle().Foreground(colorYellow)
	greenStyle    = lipgloss.NewStyle().Foreground(colorGreen)
	redStyle      = lipgloss.NewStyle().Foreground(colorRed)
	blueStyle     = lipgloss.NewStyle().Foreground(colorBlue)
	cyanStyle     = lipgloss.NewStyle().Foreground(colorCyan)
	headerStyle   = lipgloss.NewStyle().Bold(true).Foreground(colorCyan)
	yellowStyle   = lipgloss.NewStyle().Foreground(colorYellow)

	activeBorder = lipgloss.NewStyle().
			Border(lipgloss.RoundedBorder()).
			BorderForeground(colorGreen)

	inactiveBorder = lipgloss.NewStyle().
			Border(lipgloss.RoundedBorder()).
			BorderForeground(colorGray)

	activeTitleStyle   = lipgloss.NewStyle().Bold(true).Foreground(colorGreen)
	inactiveTitleStyle = lipgloss.NewStyle().Bold(true).Foreground(colorWhite)
)

// --- Bubbletea model ---

type panel int

const (
	panelPlaintext panel = iota
	panelEncrypted
	panelFiles
	panelKeys
	panelRemotes
	panelCount
)

type model struct {
	activePanel      panel
	selected         [panelCount]int
	plaintextCommits []Commit
	encryptedCommits []Commit
	files            []FileChange
	commitDiff       string
	commitMeta       string
	keys             []Key
	remotes          []Remote
	config           Config
	commitMap        *CommitMap
	prereqs          PrereqStatus
	width            int
	height           int
	showHelp         bool
	statusMsg        string
	confirmRekey     bool
	syncing          bool
	diffScroll       int
	commandLog       []string

	showRemoteInput bool
	remoteInputStep int
	remoteInputName string
	textInput       textinput.Model

	showDecryptInput bool
}

// --- Tea messages ---

type initFinishedMsg struct {
	err    error
	output string
}

type syncFinishedMsg struct {
	err         error
	syncedCount int
	output      string
}

type rekeyFinishedMsg struct {
	err    error
	output string
}

type pushFinishedMsg struct {
	err    error
	output string
}

type pullDecryptFinishedMsg struct {
	err            error
	decryptedCount int
}

type remoteAddedMsg struct {
	err  error
	name string
	url  string
}

type dataReloadedMsg struct{}

func initialModel() model {
	prereqs := checkPrereqs()
	ti := textinput.New()
	ti.Placeholder = "Enter value..."
	cfg, _ := loadConfig()
	m := model{
		activePanel: panelPlaintext,
		prereqs:     prereqs,
		config:      cfg,
		textInput:   ti,
	}
	if prereqs.Initialized {
		m.plaintextCommits = getPlaintextCommits()
		m.encryptedCommits = getEncryptedCommits()
		m.keys = getKeys()
		m.remotes = getRemotes()
		m.commitMap = loadCommitMap(commitMapPath())
		if len(m.plaintextCommits) > 0 {
			sha := m.plaintextCommits[0].SHA
			m.files = getFilesForCommit(sha)
			m.commitMeta = getCommitMeta(sha)
			m.commitDiff = getCommitDiff(sha)
		}
	}
	return m
}

func (m model) Init() tea.Cmd {
	return nil
}

func (m model) reloadData() tea.Cmd {
	return func() tea.Msg {
		return dataReloadedMsg{}
	}
}

func (m model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	m.commandLog = snapshotCmdLog()

	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height
		return m, nil

	case dataReloadedMsg:
		m.prereqs = checkPrereqs()
		m.config, _ = loadConfig()
		m.plaintextCommits = getPlaintextCommits()
		m.encryptedCommits = getEncryptedCommits()
		m.keys = getKeys()
		m.remotes = getRemotes()
		m.commitMap = loadCommitMap(commitMapPath())
		if len(m.plaintextCommits) > 0 {
			idx := m.selected[panelPlaintext]
			if idx >= len(m.plaintextCommits) {
				idx = 0
			}
			sha := m.plaintextCommits[idx].SHA
			m.files = getFilesForCommit(sha)
			m.commitMeta = getCommitMeta(sha)
			m.commitDiff = getCommitDiff(sha)
			m.diffScroll = 0
		}
		return m, nil

	case initFinishedMsg:
		if msg.err != nil {
			m.statusMsg = fmt.Sprintf("Init failed: %v", msg.err)
		} else {
			m.statusMsg = "Initialized lazycrypt"
		}
		return m, m.reloadData()

	case syncFinishedMsg:
		m.syncing = false
		if msg.err != nil {
			m.statusMsg = fmt.Sprintf("Sync failed: %v", msg.err)
		} else {
			m.statusMsg = fmt.Sprintf("Synced %d commits", msg.syncedCount)
		}
		return m, m.reloadData()

	case rekeyFinishedMsg:
		if msg.err != nil {
			m.statusMsg = fmt.Sprintf("Re-key failed: %v", msg.err)
		} else {
			m.statusMsg = "Re-keyed successfully, encrypted history rebuilt"
		}
		return m, m.reloadData()

	case pushFinishedMsg:
		if msg.err != nil {
			m.statusMsg = fmt.Sprintf("Push failed: %v", msg.err)
		} else {
			m.statusMsg = "Pushed encrypted repo to remote"
		}
		return m, nil

	case remoteAddedMsg:
		if msg.err != nil {
			m.statusMsg = fmt.Sprintf("Remote add failed: %v", msg.err)
		} else {
			m.statusMsg = fmt.Sprintf("Added encrypted remote: %s -> %s", msg.name, msg.url)
		}
		return m, m.reloadData()

	case pullDecryptFinishedMsg:
		if msg.err != nil {
			m.statusMsg = fmt.Sprintf("Pull & decrypt failed: %v", msg.err)
		} else {
			m.statusMsg = fmt.Sprintf("Decrypted %d commits into plaintext history", msg.decryptedCount)
		}
		return m, m.reloadData()

	case tea.KeyMsg:
		if m.showHelp {
			if key.Matches(msg, key.NewBinding(key.WithKeys("?", "esc"))) {
				m.showHelp = false
			}
			if key.Matches(msg, key.NewBinding(key.WithKeys("q", "ctrl+c"))) {
				return m, tea.Quit
			}
			return m, nil
		}
		if m.showRemoteInput {
			return m.updateRemoteInput(msg)
		}
		if m.showDecryptInput {
			return m.updateDecryptInput(msg)
		}
		if m.confirmRekey {
			if key.Matches(msg, key.NewBinding(key.WithKeys("R"))) {
				m.confirmRekey = false
				m.statusMsg = "Re-keying..."
				return m, m.performRekey()
			}
			m.confirmRekey = false
			m.statusMsg = ""
			return m, nil
		}
		return m.handleKeypress(msg)
	}

	return m, nil
}

func (m model) handleKeypress(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch {
	case key.Matches(msg, key.NewBinding(key.WithKeys("q", "ctrl+c"))):
		return m, tea.Quit

	case key.Matches(msg, key.NewBinding(key.WithKeys("?"))):
		m.showHelp = !m.showHelp
		return m, nil

	case key.Matches(msg, key.NewBinding(key.WithKeys("tab"))):
		m.activePanel = (m.activePanel + 1) % panelCount
		return m, nil

	case key.Matches(msg, key.NewBinding(key.WithKeys("shift+tab"))):
		m.activePanel = (m.activePanel - 1 + panelCount) % panelCount
		return m, nil

	case key.Matches(msg, key.NewBinding(key.WithKeys("j", "down"))):
		if m.activePanel == panelFiles {
			m.diffScroll++
		} else {
			m.moveDown()
		}
		return m, nil

	case key.Matches(msg, key.NewBinding(key.WithKeys("k", "up"))):
		if m.activePanel == panelFiles {
			if m.diffScroll > 0 {
				m.diffScroll--
			}
		} else {
			m.moveUp()
		}
		return m, nil

	case key.Matches(msg, key.NewBinding(key.WithKeys("i"))):
		if !m.prereqs.Initialized {
			m.statusMsg = "Initializing..."
			return m, m.performInit()
		}
		m.statusMsg = "Already initialized"
		return m, nil

	case key.Matches(msg, key.NewBinding(key.WithKeys("s"))):
		if !m.prereqs.Initialized {
			m.statusMsg = "Not initialized. Press [i] first."
			return m, nil
		}
		if m.syncing {
			return m, nil
		}
		m.syncing = true
		m.statusMsg = "Syncing..."
		return m, m.performSync()

	case key.Matches(msg, key.NewBinding(key.WithKeys("p"))):
		if !m.prereqs.Initialized {
			m.statusMsg = "Not initialized. Press [i] first."
			return m, nil
		}
		m.statusMsg = "Pushing..."
		return m, m.performPush()

	case key.Matches(msg, key.NewBinding(key.WithKeys("R"))):
		if !m.prereqs.Initialized {
			m.statusMsg = "Not initialized. Press [i] first."
			return m, nil
		}
		m.confirmRekey = true
		m.statusMsg = "Press R again to confirm re-key (rebuilds encrypted history)"
		return m, nil

	case key.Matches(msg, key.NewBinding(key.WithKeys("r"))):
		m.statusMsg = "Reloading..."
		return m, m.reloadData()

	case key.Matches(msg, key.NewBinding(key.WithKeys("e"))):
		if !m.prereqs.Initialized {
			m.statusMsg = "Not initialized. Press [i] first."
			return m, nil
		}
		m.showRemoteInput = true
		m.remoteInputStep = 0
		m.textInput.SetValue("")
		m.textInput.Placeholder = "lazycrypted-remote"
		m.textInput.Focus()
		return m, nil

	case key.Matches(msg, key.NewBinding(key.WithKeys("D"))):
		m.showDecryptInput = true
		m.textInput.SetValue("")
		m.textInput.Placeholder = ".lazycrypt/keys/current.key"
		m.textInput.Focus()
		return m, nil
	}

	return m, nil
}

func (m *model) moveDown() {
	max := m.panelItemCount()
	if max > 0 && m.selected[m.activePanel] < max-1 {
		m.selected[m.activePanel]++
		m.updateFilesForSelection()
	}
}

func (m *model) moveUp() {
	if m.selected[m.activePanel] > 0 {
		m.selected[m.activePanel]--
		m.updateFilesForSelection()
	}
}

func (m *model) updateFilesForSelection() {
	if m.activePanel == panelPlaintext && len(m.plaintextCommits) > 0 {
		idx := m.selected[panelPlaintext]
		if idx < len(m.plaintextCommits) {
			sha := m.plaintextCommits[idx].SHA
			m.files = getFilesForCommit(sha)
			m.commitMeta = getCommitMeta(sha)
			m.commitDiff = getCommitDiff(sha)
			m.diffScroll = 0
		}
	}
}

func (m model) panelItemCount() int {
	switch m.activePanel {
	case panelPlaintext:
		return len(m.plaintextCommits)
	case panelEncrypted:
		return len(m.encryptedCommits)
	case panelFiles:
		return len(m.files)
	case panelKeys:
		return len(m.keys)
	case panelRemotes:
		return len(m.remotes)
	}
	return 0
}

// --- Commands ---

func (m model) performInit() tea.Cmd {
	return func() tea.Msg {
		for _, dir := range []string{lazycryptDir(), keysDir()} {
			if err := os.MkdirAll(dir, 0755); err != nil {
				return initFinishedMsg{err: fmt.Errorf("mkdir %s: %w", dir, err)}
			}
		}

		cmd := loggedCommand("age-keygen", "-o", currentKeyPath())
		if out, err := cmd.CombinedOutput(); err != nil {
			return initFinishedMsg{err: fmt.Errorf("age-keygen: %s: %w", string(out), err)}
		}

		if err := os.Chmod(currentKeyPath(), keyFilePerms); err != nil {
			return initFinishedMsg{err: fmt.Errorf("chmod key: %w", err)}
		}

		cmd = loggedCommand("git", "init", "--bare", encryptedRepoPath())
		if out, err := cmd.CombinedOutput(); err != nil {
			return initFinishedMsg{err: fmt.Errorf("git init: %s: %w", string(out), err)}
		}

		if err := os.WriteFile(commitMapPath(), []byte(""), 0644); err != nil {
			return initFinishedMsg{err: fmt.Errorf("commit-map: %w", err)}
		}

		cfg := Config{
			Version:    1,
			CurrentKey: currentKeyPath(),
		}
		if err := saveConfig(cfg); err != nil {
			return initFinishedMsg{err: fmt.Errorf("config: %w", err)}
		}

		if err := addToGitignore(".lazycrypt"); err != nil {
			return initFinishedMsg{err: fmt.Errorf("gitignore: %w", err)}
		}

		return initFinishedMsg{}
	}
}

func addToGitignore(pattern string) error {
	content, _ := os.ReadFile(".gitignore")
	if strings.Contains(string(content), pattern) {
		return nil
	}
	f, err := os.OpenFile(".gitignore", os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		return fmt.Errorf("open .gitignore: %w", err)
	}
	defer f.Close()
	prefix := ""
	if len(content) > 0 && content[len(content)-1] != '\n' {
		prefix = "\n"
	}
	_, err = f.WriteString(prefix + pattern + "\n")
	return err
}

func (m model) performSync() tea.Cmd {
	return func() tea.Msg {
		pubKey := getPublicKey(currentKeyPath())
		if pubKey == "" {
			return syncFinishedMsg{err: fmt.Errorf("no public key found in %s", currentKeyPath())}
		}

		cfg, err := loadConfig()
		if err != nil {
			return syncFinishedMsg{err: fmt.Errorf("load config: %w", err)}
		}
		cm := loadCommitMap(commitMapPath())

		out, err := loggedCommand("git", "log", "--format=%H", "--reverse").Output()
		if err != nil {
			return syncFinishedMsg{err: fmt.Errorf("git log: %w", err)}
		}

		shas := splitNonEmpty(string(out))
		synced := 0
		absEncRepo, err := filepath.Abs(encryptedRepoPath())
		if err != nil {
			return syncFinishedMsg{err: fmt.Errorf("resolve encrypted repo path: %w", err)}
		}

		for _, sha := range shas {
			if cm.isSynced(sha) {
				continue
			}
			encSHA, err := syncOneCommit(sha, absEncRepo, pubKey, cm, cfg.ExcludePatterns)
			if err != nil {
				cm.save()
				return syncFinishedMsg{err: fmt.Errorf("commit %s: %w", sha, err), syncedCount: synced}
			}
			cm.add(sha, encSHA)
			synced++
		}

		if err := cm.save(); err != nil {
			return syncFinishedMsg{err: fmt.Errorf("save commit-map: %w", err), syncedCount: synced}
		}

		return syncFinishedMsg{syncedCount: synced}
	}
}

// commitMetaInfo holds author, committer, and date information extracted from a git commit.
type commitMetaInfo struct {
	message       string
	author        string
	date          string
	committerName string
	committerEmail string
	committerDate string
}

// getCommitMetaInfo extracts commit message, author, committer, and date from
// a commit in a single git call, optionally in a specific repo.
func getCommitMetaInfo(sha string, repoArgs ...string) (commitMetaInfo, error) {
	format := "%s%x00%an <%ae>%x00%ai%x00%cn%x00%ce%x00%ci"
	args := append([]string{}, repoArgs...)
	args = append(args, "log", "-1", "--format="+format, sha)
	out, err := loggedCommand("git", args...).Output()
	if err != nil {
		return commitMetaInfo{}, fmt.Errorf("git log metadata for %s: %w", sha, err)
	}
	parts := strings.SplitN(strings.TrimRight(string(out), "\n"), "\x00", 6)
	if len(parts) < 6 {
		return commitMetaInfo{}, fmt.Errorf("unexpected git log output for %s: got %d fields, want 6", sha, len(parts))
	}
	msg := parts[0]
	if msg == "" {
		msg = "(no message)"
	}
	return commitMetaInfo{
		message:        msg,
		author:         parts[1],
		date:           parts[2],
		committerName:  parts[3],
		committerEmail: parts[4],
		committerDate:  parts[5],
	}, nil
}

// gitCommitInRepo stages all changes and creates a commit in the given repo,
// preserving the original author, committer, and date. Uses explicit
// --git-dir and --work-tree so the bare repo and work tree are unambiguous
// (avoids issues with multiple files or env not propagating to child process).
func gitCommitInRepo(workDir, gitDir string, meta commitMetaInfo, envExtra ...string) (string, error) {
	absWorkDir, err := filepath.Abs(workDir)
	if err != nil {
		return "", fmt.Errorf("resolve work dir: %w", err)
	}
	absGitDir, err := filepath.Abs(gitDir)
	if err != nil {
		return "", fmt.Errorf("resolve git dir: %w", err)
	}

	env := append(os.Environ(), envExtra...)
	if meta.committerName != "" {
		env = append(env, "GIT_COMMITTER_NAME="+meta.committerName)
	}
	if meta.committerEmail != "" {
		env = append(env, "GIT_COMMITTER_EMAIL="+meta.committerEmail)
	}
	if meta.committerDate != "" {
		env = append(env, "GIT_COMMITTER_DATE="+meta.committerDate)
	}

	addArgs := []string{"--git-dir=" + absGitDir, "--work-tree=" + absWorkDir, "add", "-A"}
	addCmd := loggedCommand("git", addArgs...)
	addCmd.Env = env
	if out, err := addCmd.CombinedOutput(); err != nil {
		return "", fmt.Errorf("git add: %s: %w", string(out), err)
	}

	commitArgs := []string{"--git-dir=" + absGitDir, "--work-tree=" + absWorkDir, "commit", "--allow-empty", "-m", meta.message}
	if meta.author != "" {
		commitArgs = append(commitArgs, "--author", meta.author)
	}
	if meta.date != "" {
		commitArgs = append(commitArgs, "--date", meta.date)
	}
	commitCmd := loggedCommand("git", commitArgs...)
	commitCmd.Env = env
	if out, err := commitCmd.CombinedOutput(); err != nil {
		return "", fmt.Errorf("git commit: %s: %w", string(out), err)
	}

	shaCmd := loggedCommand("git", "rev-parse", "HEAD")
	shaCmd.Env = append(os.Environ(), "GIT_DIR="+absGitDir)
	shaOut, err := shaCmd.Output()
	if err != nil {
		return "", fmt.Errorf("rev-parse: %w", err)
	}
	return strings.TrimSpace(string(shaOut)), nil
}

func syncOneCommit(sha, encRepoPath, pubKey string, cm *CommitMap, excludePatterns []string) (string, error) {
	tmpDir, err := os.MkdirTemp("", "lazycrypt-sync-")
	if err != nil {
		return "", fmt.Errorf("mktemp: %w", err)
	}
	defer os.RemoveAll(tmpDir)

	parentOut, _ := loggedCommand("git", "rev-list", "--parents", "-n", "1", sha).Output()
	parentFields := strings.Fields(strings.TrimSpace(string(parentOut)))
	encParent := ""
	if len(parentFields) > 1 {
		if enc, ok := cm.mapping[parentFields[1]]; ok {
			encParent = enc
		}
	}

	gitEnv := []string{
		"GIT_DIR=" + encRepoPath,
		"GIT_WORK_TREE=" + tmpDir,
	}

	if encParent != "" {
		cmd := loggedCommand("git", "checkout", encParent, "--", ".")
		cmd.Dir = tmpDir
		cmd.Env = append(os.Environ(), gitEnv...)
		if out, err := cmd.CombinedOutput(); err != nil {
			msg := string(out)
			// Tolerate "did not match any file(s)" errors: this happens
			// when the parent encrypted commit has an empty tree (e.g. all
			// files were excluded). The checkout is best-effort; we'll
			// overwrite the work tree with encrypted blobs next anyway.
			if !strings.Contains(msg, "did not match any file(s) known to git") {
				return "", fmt.Errorf("checkout parent: %s: %w", msg, err)
			}
		}
	}

	isRoot := encParent == ""
	diffOut, err := getCommitDiffTree(sha, isRoot)
	if err != nil {
		return "", fmt.Errorf("diff-tree: %w", err)
	}

	changes := parseNameStatusZ(diffOut)
	for _, change := range changes {
		filePath := change.Path
		if matchesExcludePattern(filePath, excludePatterns) {
			continue
		}

		if change.Status == "" {
			continue
		}
		switch change.Status[0] {
		case 'A', 'M', 'R':
			content, err := readBlobAtCommit(sha, filePath)
			if err == errSubmoduleEntry {
				logCmd("skipping submodule entry: %s", filePath)
				continue
			}
			if err != nil {
				return "", fmt.Errorf("read blob %s at %s: %w", filePath, sha, err)
			}
			encrypted, err := ageEncrypt(content, pubKey)
			if err != nil {
				return "", fmt.Errorf("encrypt %s: %w", filePath, err)
			}
			destPath := filepath.Join(tmpDir, filePath)
			if err := os.MkdirAll(filepath.Dir(destPath), 0755); err != nil {
				return "", err
			}
			if err := os.WriteFile(destPath, encrypted, 0644); err != nil {
				return "", err
			}
		case 'D':
			if err := os.Remove(filepath.Join(tmpDir, filePath)); err != nil && !os.IsNotExist(err) {
				return "", fmt.Errorf("remove %s: %w", filePath, err)
			}
		}
	}

	meta, err := getCommitMetaInfo(sha)
	if err != nil {
		return "", fmt.Errorf("metadata: %w", err)
	}
	return gitCommitInRepo(tmpDir, encRepoPath, meta, gitEnv...)
}

// readBlobAtCommit returns the contents of filePath as of commit sha.
// It first resolves the blob ID with ls-tree + cat-file (robust for unusual
// filenames), and falls back to "git show sha:path" if needed.
// Returns nil, errSubmoduleEntry when the path is a submodule pointer.
func readBlobAtCommit(sha, filePath string) ([]byte, error) {
	blobID, err := getBlobIDForPath(sha, filePath)
	if err == errSubmoduleEntry {
		return nil, errSubmoduleEntry
	}
	if err == nil {
		out, blobErr := loggedCommand("git", "cat-file", "blob", blobID).Output()
		if blobErr == nil {
			return out, nil
		}
		if exitErr, ok := blobErr.(*exec.ExitError); ok {
			err = fmt.Errorf("git cat-file blob %s: %s", blobID, strings.TrimSpace(string(exitErr.Stderr)))
		} else {
			err = fmt.Errorf("git cat-file blob %s: %w", blobID, blobErr)
		}
	}

	showOut, showErr := loggedCommand("git", "show", sha+":"+filePath).Output()
	if showErr == nil {
		return showOut, nil
	}
	if exitErr, ok := showErr.(*exec.ExitError); ok {
		return nil, fmt.Errorf("%v; git show %s:%s: %s", err, sha, filePath, strings.TrimSpace(string(exitErr.Stderr)))
	}
	return nil, fmt.Errorf("%v; git show %s:%s: %w", err, sha, filePath, showErr)
}

// errSubmoduleEntry is returned when a path refers to a submodule (type "commit")
// rather than a regular file blob. Callers should skip the entry instead of failing.
var errSubmoduleEntry = fmt.Errorf("path is a submodule entry")

// getBlobIDForPath resolves the blob object ID for path at the given commit.
// It uses "git ls-tree -z <sha> -- <path>" and parses the first (and only)
// entry, which is safe even for paths with spaces or other special characters.
// Returns errSubmoduleEntry when the entry's object type is "commit" (submodule).
func getBlobIDForPath(sha, path string) (string, error) {
	out, err := loggedCommand("git", "ls-tree", "-z", sha, "--", path).Output()
	if err != nil {
		return "", fmt.Errorf("git ls-tree: %w", err)
	}
	if len(out) == 0 {
		return "", fmt.Errorf("path %q not found in commit %s", path, sha)
	}
	parts := bytes.SplitN(out, []byte{0}, 2)
	line := parts[0]
	if len(line) == 0 {
		return "", fmt.Errorf("empty ls-tree output for %s %q", sha, path)
	}
	metaAndPath := bytes.SplitN(line, []byte("\t"), 2)
	if len(metaAndPath) != 2 {
		return "", fmt.Errorf("unexpected ls-tree line: %q", string(line))
	}
	// Fields: <mode> <type> <hash>
	metaFields := strings.Fields(string(metaAndPath[0]))
	if len(metaFields) < 3 {
		return "", fmt.Errorf("unexpected ls-tree meta: %q", metaAndPath[0])
	}
	objType := metaFields[1]
	if objType == "commit" {
		return "", errSubmoduleEntry
	}
	return metaFields[2], nil
}

// getCommitDiffTree returns the name-status output for a commit.
func getCommitDiffTree(sha string, isRoot bool, repoArgs ...string) ([]byte, error) {
	args := append(repoArgs, "diff-tree")
	if isRoot {
		args = append(args, "--root")
	}
	args = append(args, "--no-commit-id", "-r", "--name-status", "-z", sha)
	return loggedCommand("git", args...).Output()
}

func (m model) updateDecryptInput(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	var cmd tea.Cmd
	switch {
	case key.Matches(msg, key.NewBinding(key.WithKeys("esc"))):
		m.showDecryptInput = false
		m.textInput.SetValue("")
		return m, nil
	case key.Matches(msg, key.NewBinding(key.WithKeys("enter"))):
		val := m.textInput.Value()
		if val == "" {
			val = ".lazycrypt/keys/current.key"
		}
		m.showDecryptInput = false
		m.textInput.SetValue("")
		m.statusMsg = "Pulling and decrypting..."
		return m, m.performPullDecrypt(val, m.prereqs.RepoPath)
	default:
		m.textInput, cmd = m.textInput.Update(msg)
	}
	return m, cmd
}

// expandHome replaces a leading ~/ with the user's home directory.
func expandHome(path string) string {
	if strings.HasPrefix(path, "~/") {
		home, _ := os.UserHomeDir()
		return filepath.Join(home, path[2:])
	}
	return path
}

func (m model) performPullDecrypt(keyPath, repoPath string) tea.Cmd {
	return func() tea.Msg {
		keyPath = expandHome(keyPath)

		if _, err := os.Stat(keyPath); err != nil {
			return pullDecryptFinishedMsg{err: fmt.Errorf("key file not found: %s", keyPath)}
		}

		cfg, err := loadConfig()
		if err != nil {
			return pullDecryptFinishedMsg{err: fmt.Errorf("load config: %w", err)}
		}
		encRepo := encryptedRepoPath()
		absEncRepo, err := filepath.Abs(encRepo)
		if err != nil {
			return pullDecryptFinishedMsg{err: fmt.Errorf("resolve encrypted repo path: %w", err)}
		}

		if _, err := os.Stat(encRepo); os.IsNotExist(err) {
			if cfg.EncryptedRemote.URL == "" {
				return pullDecryptFinishedMsg{err: fmt.Errorf("no encrypted remote configured -- press [e] first")}
			}
			if err := os.MkdirAll(lazycryptDir(), 0755); err != nil {
				return pullDecryptFinishedMsg{err: fmt.Errorf("mkdir: %w", err)}
			}
			cmd := loggedCommand("git", "clone", "--bare", cfg.EncryptedRemote.URL, absEncRepo)
			if out, err := cmd.CombinedOutput(); err != nil {
				return pullDecryptFinishedMsg{err: fmt.Errorf("clone: %s: %w", string(out), err)}
			}
		} else if cfg.EncryptedRemote.Name != "" {
			fetchCmd := loggedCommand("git", "-C", absEncRepo, "fetch", "--all")
			if out, err := fetchCmd.CombinedOutput(); err != nil {
				return pullDecryptFinishedMsg{err: fmt.Errorf("fetch: %s: %w", string(out), err)}
			}
		}

		recvMap := loadCommitMap(recvCommitMapPath())
		recvMap.filePath = recvCommitMapPath()

		out, err := loggedCommand("git", "-C", absEncRepo, "log", "--format=%H", "--reverse", "--all").Output()
		if err != nil {
			return pullDecryptFinishedMsg{err: fmt.Errorf("git log: %w", err)}
		}

		count := 0
		for _, sha := range splitNonEmpty(string(out)) {
			if recvMap.isSynced(sha) {
				continue
			}
			plainSHA, err := decryptOneCommit(sha, absEncRepo, keyPath, repoPath)
			if err != nil {
				recvMap.save()
				return pullDecryptFinishedMsg{err: fmt.Errorf("commit %s: %w", sha[:7], err), decryptedCount: count}
			}
			recvMap.add(sha, plainSHA)
			count++
		}

		recvMap.save()
		return pullDecryptFinishedMsg{decryptedCount: count}
	}
}

func decryptOneCommit(encSHA, encRepoPath, keyPath, repoPath string) (string, error) {
	parentOut, _ := loggedCommand("git", "-C", encRepoPath, "rev-list", "--parents", "-n", "1", encSHA).Output()
	isRoot := len(strings.Fields(strings.TrimSpace(string(parentOut)))) <= 1

	diffOut, err := getCommitDiffTree(encSHA, isRoot, "-C", encRepoPath)
	if err != nil {
		return "", fmt.Errorf("diff-tree: %w", err)
	}

	// Phase 1: decrypt all files into memory before touching the working directory.
	type pendingFile struct {
		path    string
		content []byte
	}
	var toWrite []pendingFile
	var toDelete []string

	changes := parseNameStatusZ(diffOut)
	for _, change := range changes {
		filePath := change.Path
		if change.Status == "" {
			continue
		}
		switch change.Status[0] {
		case 'A', 'M', 'R':
			content, err := loggedCommand("git", "-C", encRepoPath, "show", encSHA+":"+filePath).Output()
			if err != nil {
				return "", fmt.Errorf("git show %s:%s: %w", encSHA, filePath, err)
			}
			decrypted, err := ageDecrypt(content, keyPath)
			if err != nil {
				return "", fmt.Errorf("decrypt %s: %w", filePath, err)
			}
			toWrite = append(toWrite, pendingFile{path: filePath, content: decrypted})
		case 'D':
			toDelete = append(toDelete, filePath)
		}
	}

	meta, err := getCommitMetaInfo(encSHA, "-C", encRepoPath)
	if err != nil {
		return "", fmt.Errorf("metadata: %w", err)
	}

	// Phase 2: all decryptions succeeded, apply changes to the working directory.
	for _, f := range toWrite {
		destPath := filepath.Join(repoPath, f.path)
		if err := os.MkdirAll(filepath.Dir(destPath), 0755); err != nil {
			return "", err
		}
		if err := os.WriteFile(destPath, f.content, 0644); err != nil {
			return "", err
		}
	}
	for _, p := range toDelete {
		if err := os.Remove(filepath.Join(repoPath, p)); err != nil && !os.IsNotExist(err) {
			return "", fmt.Errorf("remove %s: %w", p, err)
		}
	}

	// Phase 3: stage and commit.
	for _, f := range toWrite {
		addCmd := loggedCommand("git", "-C", repoPath, "add", "--", f.path)
		if out, err := addCmd.CombinedOutput(); err != nil {
			return "", fmt.Errorf("git add %s: %s: %w", f.path, string(out), err)
		}
	}
	for _, p := range toDelete {
		rmCmd := loggedCommand("git", "-C", repoPath, "rm", "--cached", "--ignore-unmatch", "--", p)
		if out, err := rmCmd.CombinedOutput(); err != nil {
			return "", fmt.Errorf("git rm %s: %s: %w", p, string(out), err)
		}
	}

	var commitEnv []string
	if meta.committerName != "" {
		commitEnv = append(commitEnv, "GIT_COMMITTER_NAME="+meta.committerName)
	}
	if meta.committerEmail != "" {
		commitEnv = append(commitEnv, "GIT_COMMITTER_EMAIL="+meta.committerEmail)
	}
	if meta.committerDate != "" {
		commitEnv = append(commitEnv, "GIT_COMMITTER_DATE="+meta.committerDate)
	}

	commitArgs := []string{"-C", repoPath, "commit", "--allow-empty", "-m", meta.message}
	if meta.author != "" {
		commitArgs = append(commitArgs, "--author", meta.author)
	}
	if meta.date != "" {
		commitArgs = append(commitArgs, "--date", meta.date)
	}
	commitCmd := loggedCommand("git", commitArgs...)
	commitCmd.Env = append(os.Environ(), commitEnv...)
	if out, err := commitCmd.CombinedOutput(); err != nil {
		return "", fmt.Errorf("git commit: %s: %w", string(out), err)
	}

	shaOut, err := loggedCommand("git", "-C", repoPath, "rev-parse", "HEAD").Output()
	if err != nil {
		return "", fmt.Errorf("rev-parse: %w", err)
	}
	return strings.TrimSpace(string(shaOut)), nil
}

func (m model) updateRemoteInput(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	var cmd tea.Cmd
	switch {
	case key.Matches(msg, key.NewBinding(key.WithKeys("esc"))):
		m.showRemoteInput = false
		m.textInput.SetValue("")
		return m, nil
	case key.Matches(msg, key.NewBinding(key.WithKeys("enter"))):
		val := m.textInput.Value()
		if m.remoteInputStep == 0 {
			if val == "" {
				val = "lazycrypted-remote"
			}
			m.remoteInputName = val
			m.remoteInputStep = 1
			m.textInput.SetValue("")
			defaultURL := defaultEncryptedRemoteURL()
			if defaultURL != "" {
				m.textInput.Placeholder = defaultURL
			} else {
				m.textInput.Placeholder = "git@github.com:user/repo-lazycrypted.git"
			}
			return m, nil
		}
		if val == "" {
			val = defaultEncryptedRemoteURL()
		}
		if val == "" {
			m.statusMsg = "No URL provided and could not determine default"
			return m, nil
		}
		m.showRemoteInput = false
		name := m.remoteInputName
		url := val
		m.textInput.SetValue("")
		m.statusMsg = fmt.Sprintf("Adding encrypted remote %s...", name)
		return m, m.addEncryptedRemote(name, url)
	default:
		m.textInput, cmd = m.textInput.Update(msg)
	}
	return m, cmd
}

func (m model) addEncryptedRemote(name, url string) tea.Cmd {
	return func() tea.Msg {
		cmd := loggedCommand("git", "-C", encryptedRepoPath(), "remote", "add", name, url)
		if _, err := cmd.CombinedOutput(); err != nil {
			cmd = loggedCommand("git", "-C", encryptedRepoPath(), "remote", "set-url", name, url)
			if out, err := cmd.CombinedOutput(); err != nil {
				return remoteAddedMsg{err: fmt.Errorf("git remote: %s", string(out))}
			}
		}

		cfg, err := loadConfig()
		if err != nil {
			return remoteAddedMsg{err: fmt.Errorf("load config: %w", err)}
		}
		cfg.EncryptedRemote = RemoteConfig{Name: name, URL: url}
		if err := saveConfig(cfg); err != nil {
			return remoteAddedMsg{err: fmt.Errorf("save config: %w", err)}
		}
		return remoteAddedMsg{name: name, url: url}
	}
}

func (m model) performPush() tea.Cmd {
	return func() tea.Msg {
		cfg, err := loadConfig()
		if err != nil {
			return pushFinishedMsg{err: fmt.Errorf("load config: %w", err)}
		}
		remoteName := cfg.EncryptedRemote.Name
		if remoteName == "" {
			return pushFinishedMsg{err: fmt.Errorf("no encrypted remote configured in .lazycrypt/config.yml")}
		}

		// Force push: the encrypted repo is a derived history, not collaborative.
		cmd := loggedCommand("git", "-C", encryptedRepoPath(), "push", "--force", remoteName, "--all")
		out, err := cmd.CombinedOutput()
		if err != nil {
			return pushFinishedMsg{err: fmt.Errorf("%s: %w", string(out), err)}
		}
		return pushFinishedMsg{}
	}
}

func (m model) performRekey() tea.Cmd {
	return func() tea.Msg {
		timestamp := time.Now().Format("2006-01-02-150405")
		retiredPath := filepath.Join(keysDir(), "retired-"+timestamp+".key")
		if err := os.Rename(currentKeyPath(), retiredPath); err != nil {
			return rekeyFinishedMsg{err: fmt.Errorf("retire key: %w", err)}
		}

		cfg, err := loadConfig()
		if err != nil {
			return rekeyFinishedMsg{err: fmt.Errorf("load config: %w", err)}
		}
		cfg.RetiredKeys = append(cfg.RetiredKeys, RetiredKey{
			Path:      retiredPath,
			RetiredAt: time.Now().Format(time.RFC3339),
		})

		cmd := loggedCommand("age-keygen", "-o", currentKeyPath())
		if out, err := cmd.CombinedOutput(); err != nil {
			os.Rename(retiredPath, currentKeyPath())
			return rekeyFinishedMsg{err: fmt.Errorf("age-keygen: %s: %w", string(out), err)}
		}

		if err := os.Chmod(currentKeyPath(), keyFilePerms); err != nil {
			return rekeyFinishedMsg{err: fmt.Errorf("chmod key: %w", err)}
		}

		cfg.CurrentKey = currentKeyPath()
		if err := saveConfig(cfg); err != nil {
			return rekeyFinishedMsg{err: fmt.Errorf("save config: %w", err)}
		}

		os.RemoveAll(encryptedRepoPath())
		os.Remove(commitMapPath())

		cmd = loggedCommand("git", "init", "--bare", encryptedRepoPath())
		if out, err := cmd.CombinedOutput(); err != nil {
			return rekeyFinishedMsg{err: fmt.Errorf("git init: %s: %w", string(out), err)}
		}

		if err := os.WriteFile(commitMapPath(), []byte(""), 0644); err != nil {
			return rekeyFinishedMsg{err: fmt.Errorf("commit-map: %w", err)}
		}

		pubKey := getPublicKey(currentKeyPath())
		if pubKey == "" {
			return rekeyFinishedMsg{err: fmt.Errorf("no public key in new key")}
		}

		cfg, err = loadConfig()
		if err != nil {
			return rekeyFinishedMsg{err: fmt.Errorf("reload config: %w", err)}
		}
		cm := loadCommitMap(commitMapPath())
		allOut, err := loggedCommand("git", "log", "--format=%H", "--reverse").Output()
		if err != nil {
			return rekeyFinishedMsg{err: fmt.Errorf("git log: %w", err)}
		}

		absEncRepo, err := filepath.Abs(encryptedRepoPath())
		if err != nil {
			return rekeyFinishedMsg{err: fmt.Errorf("resolve encrypted repo path: %w", err)}
		}
		for _, sha := range splitNonEmpty(string(allOut)) {
			encSHA, err := syncOneCommit(sha, absEncRepo, pubKey, cm, cfg.ExcludePatterns)
			if err != nil {
				cm.save()
				return rekeyFinishedMsg{err: fmt.Errorf("re-sync %s: %w", sha, err)}
			}
			cm.add(sha, encSHA)
		}
		cm.save()

		return rekeyFinishedMsg{}
	}
}

// --- Utility ---

// splitNonEmpty splits a string by newlines and returns only non-empty entries.
func splitNonEmpty(s string) []string {
	var result []string
	for _, line := range strings.Split(strings.TrimSpace(s), "\n") {
		if line != "" {
			result = append(result, line)
		}
	}
	return result
}

func truncate(s string, max int) string {
	if max <= 3 {
		return s
	}
	if len(s) <= max {
		return s
	}
	return s[:max-3] + "..."
}

// --- View ---

func (m model) View() string {
	if m.width == 0 {
		return "Loading..."
	}
	if m.showHelp {
		return m.renderHelp()
	}
	if m.showRemoteInput {
		return m.renderRemoteInput()
	}
	if m.showDecryptInput {
		return m.renderDecryptInput()
	}
	if !m.prereqs.Initialized {
		return m.renderNotInitialized()
	}
	return m.renderMain()
}

func (m model) renderDecryptInput() string {
	var b strings.Builder
	b.WriteString(headerStyle.Render("Receiver: Pull & Decrypt Encrypted History") + "\n\n")
	b.WriteString("This will:\n")
	b.WriteString(fmt.Sprintf("  1. Fetch the encrypted remote %s\n", faintStyle.Render("(or clone if first time)")))
	b.WriteString("  2. Decrypt each commit's files using your age private key\n")
	b.WriteString("  3. Create a plaintext git history in this repo\n\n")
	b.WriteString("Enter the path to the age private key:\n\n")
	b.WriteString(m.textInput.View() + "\n\n")
	b.WriteString(faintStyle.Render("The key was shared by the sender (e.g., sender's .lazycrypt/keys/current.key)\n"))
	b.WriteString(faintStyle.Render("leave blank and press enter for default\n"))
	b.WriteString(faintStyle.Render("enter confirm | esc cancel"))
	modal := activeBorder.Width(70).Render(b.String())
	return lipgloss.Place(m.width, m.height, lipgloss.Center, lipgloss.Center, modal)
}

func (m model) renderRemoteInput() string {
	var b strings.Builder
	b.WriteString(headerStyle.Render("Configure Encrypted Remote") + "\n\n")

	if m.remoteInputStep == 0 {
		b.WriteString("Enter a name for the encrypted remote:\n\n")
	} else {
		b.WriteString(fmt.Sprintf("Remote name: %s\n", greenStyle.Render(m.remoteInputName)))
		b.WriteString("Enter the remote URL:\n\n")
	}

	b.WriteString(m.textInput.View() + "\n\n")
	b.WriteString(faintStyle.Render("leave blank and press enter for default"))
	b.WriteString("\n")
	b.WriteString(faintStyle.Render("enter confirm | esc cancel"))

	modal := activeBorder.Width(70).Render(b.String())
	return lipgloss.Place(m.width, m.height, lipgloss.Center, lipgloss.Center, modal)
}

// --- Layout helpers ---

func clipLines(lines []string, max, focus int) ([]string, int) {
	if len(lines) <= max {
		return lines, 0
	}
	start := focus - max/2
	if start < 0 {
		start = 0
	}
	end := start + max
	if end > len(lines) {
		end = len(lines)
		start = end - max
		if start < 0 {
			start = 0
		}
	}
	return lines[start:end], start
}

func clipLinesTail(lines []string, max int) []string {
	if len(lines) <= max {
		return lines
	}
	return lines[len(lines)-max:]
}

func scrollIndicators(visible []string, offset, total, maxLines int) string {
	if total <= maxLines {
		return strings.Join(visible, "\n")
	}
	below := total - offset - len(visible)
	if offset > 0 && len(visible) > 0 {
		visible[0] = faintStyle.Render(fmt.Sprintf("  ^ %d more", offset))
	}
	if below > 0 && len(visible) > 1 {
		visible[len(visible)-1] = faintStyle.Render(fmt.Sprintf("  v %d more", below))
	}
	return strings.Join(visible, "\n")
}

func (m model) renderMain() string {
	leftW := m.width / 2
	rightW := m.width - leftW

	total := len(m.plaintextCommits)
	syncedN := 0
	if m.commitMap != nil {
		syncedN = m.commitMap.syncedCount()
	}

	repoName := filepath.Base(m.prereqs.RepoPath)
	syncColor := greenStyle
	if syncedN < total {
		syncColor = yellowStyle
	}
	header := headerStyle.Render("lazycrypt") +
		faintStyle.Render(" --- ") +
		boldStyle.Render(fmt.Sprintf("repo: %s", repoName)) +
		faintStyle.Render("  ") +
		cyanStyle.Render(m.prereqs.CurrentBranch) +
		faintStyle.Render(" --- ") +
		syncColor.Render(fmt.Sprintf("synced: %d/%d", syncedN, total))

	availH := m.height - 3
	if availH < 6 {
		availH = 6
	}

	plaintextH := max(availH*35/100, 3)
	encryptedH := max(availH*25/100, 3)
	cmdLogH := max(availH-plaintextH-encryptedH, 3)

	pInner := plaintextH - 3
	eInner := encryptedH - 3
	clInner := cmdLogH - 3

	plaintextPanel := m.renderPanel("Plaintext Commits", m.renderPlaintextList(pInner, leftW-4), leftW-2, plaintextH, panelPlaintext)
	encryptedPanel := m.renderPanel("Encrypted Commits", m.renderEncryptedList(eInner, leftW-4), leftW-2, encryptedH, panelEncrypted)
	cmdLogPanel := m.renderPanel("Command Log", m.renderCommandLog(clInner, leftW-4), leftW-2, cmdLogH, -1)

	leftCol := lipgloss.JoinVertical(lipgloss.Left, plaintextPanel, encryptedPanel, cmdLogPanel)

	detailsH := max(availH*60/100, 4)
	keysH := max(availH*20/100, 3)
	remotesH := max(availH-detailsH-keysH, 3)

	dInner := detailsH - 3
	kInner := keysH - 3
	rInner := remotesH - 3

	detailsPanel := m.renderPanel("Commit Details", m.renderCommitDetails(dInner, rightW-4), rightW-2, detailsH, panelFiles)
	keysPanel := m.renderPanel("Keys", m.renderKeysList(kInner), rightW-2, keysH, panelKeys)
	remotesPanel := m.renderPanel("Remotes", m.renderRemotesList(rInner), rightW-2, remotesH, panelRemotes)

	rightCol := lipgloss.JoinVertical(lipgloss.Left, detailsPanel, keysPanel, remotesPanel)

	body := lipgloss.JoinHorizontal(lipgloss.Top, leftCol, rightCol)
	return header + "\n" + body + "\n" + m.renderFooter()
}

func (m model) renderPanel(title, content string, width, height int, p panel) string {
	titleStr := inactiveTitleStyle.Render(title)
	border := inactiveBorder
	if panel(p) == m.activePanel {
		titleStr = activeTitleStyle.Render(title)
		border = activeBorder
	}

	contentAreaH := max(height-2, 1)
	maxContentLines := max(contentAreaH-1, 0)
	contentLines := strings.Split(content, "\n")
	if len(contentLines) > maxContentLines {
		contentLines = contentLines[:maxContentLines]
	}

	inner := titleStr + "\n" + strings.Join(contentLines, "\n")

	rendered := border.
		Width(width).
		Height(contentAreaH).
		Render(inner)

	outLines := strings.Split(rendered, "\n")
	if len(outLines) > height {
		outLines = outLines[:height]
	}
	return strings.Join(outLines, "\n")
}

// renderCommitList renders a scrollable list of commits, shared between
// plaintext and encrypted panels.
func (m model) renderCommitList(commits []Commit, maxLines, maxW int, p panel, showSyncIndicator bool) string {
	if len(commits) == 0 {
		return faintStyle.Render("No commits")
	}
	var allLines []string
	for i, c := range commits {
		isSelected := m.activePanel == p && i == m.selected[p]

		var line string
		if isSelected {
			indicator := "  "
			if showSyncIndicator {
				if c.Synced {
					indicator = "v "
				} else {
					indicator = "~ "
				}
			}
			line = selectedStyle.Render(fmt.Sprintf("%s%s %s", indicator, c.ShortSHA, truncate(c.Message, maxW-14)))
		} else {
			sha := shaStyle.Render(c.ShortSHA)
			msg := truncate(c.Message, maxW-12)
			if showSyncIndicator {
				var indicator string
				if c.Synced {
					indicator = greenStyle.Render("v")
				} else {
					indicator = yellowStyle.Render("~")
				}
				line = fmt.Sprintf("%s %s %s", indicator, sha, msg)
			} else {
				line = fmt.Sprintf("  %s %s", sha, msg)
			}
		}
		allLines = append(allLines, line)
	}

	visible, offset := clipLines(allLines, maxLines, m.selected[p])
	return scrollIndicators(visible, offset, len(allLines), maxLines)
}

func (m model) renderPlaintextList(maxLines, maxW int) string {
	content := m.renderCommitList(m.plaintextCommits, maxLines, maxW, panelPlaintext, true)
	return content
}

func (m model) renderEncryptedList(maxLines, maxW int) string {
	if len(m.encryptedCommits) == 0 {
		pending := len(m.plaintextCommits)
		if pending > 0 {
			return yellowStyle.Render(fmt.Sprintf("~ %d commits pending sync ~", pending))
		}
		return faintStyle.Render("No encrypted commits")
	}
	content := m.renderCommitList(m.encryptedCommits, maxLines, maxW, panelEncrypted, false)

	syncedN := 0
	if m.commitMap != nil {
		syncedN = m.commitMap.syncedCount()
	}
	pending := len(m.plaintextCommits) - syncedN
	if pending > 0 {
		content += "\n" + yellowStyle.Render(fmt.Sprintf("~ %d commits pending sync ~", pending))
	}
	return content
}

func (m model) renderCommitDetails(maxLines, maxW int) string {
	if len(m.plaintextCommits) == 0 {
		return faintStyle.Render("No commits to display")
	}

	var content strings.Builder

	if len(m.files) > 0 {
		content.WriteString(boldStyle.Render("Changed files:") + "\n")
		for _, f := range m.files {
			var statusStr string
			switch f.Status {
			case "A":
				statusStr = greenStyle.Render("A")
			case "M":
				statusStr = yellowStyle.Render("M")
			case "D":
				statusStr = redStyle.Render("D")
			default:
				statusStr = f.Status
			}
			content.WriteString(fmt.Sprintf("  %s %s\n", statusStr, f.Path))
		}
		content.WriteString("\n")
	}

	if m.commitDiff != "" {
		content.WriteString(m.commitDiff)
	}

	lines := strings.Split(content.String(), "\n")

	needsScrollBar := len(lines) > maxLines
	visibleLines := maxLines
	if needsScrollBar && visibleLines > 1 {
		visibleLines--
	}

	maxScroll := len(lines) - visibleLines
	if maxScroll < 0 {
		maxScroll = 0
	}
	start := m.diffScroll
	if start > maxScroll {
		start = maxScroll
	}
	end := start + visibleLines
	if end > len(lines) {
		end = len(lines)
	}
	visible := lines[start:end]

	result := strings.Join(visible, "\n")
	if needsScrollBar {
		scrollInfo := faintStyle.Render(fmt.Sprintf("[%d/%d lines, j/k to scroll]", start+1, len(lines)))
		result = scrollInfo + "\n" + result
	}
	return result
}

func (m model) renderKeysList(maxLines int) string {
	if len(m.keys) == 0 {
		return faintStyle.Render("No keys -- press [i] to init")
	}
	var allLines []string
	for i, k := range m.keys {
		pubDisplay := k.PublicKey
		if len(pubDisplay) > 16 {
			pubDisplay = pubDisplay[:16] + "..."
		}
		var line string
		if m.activePanel == panelKeys && i == m.selected[panelKeys] {
			if k.Active {
				line = selectedStyle.Render(fmt.Sprintf("* current %s", pubDisplay))
			} else {
				line = selectedStyle.Render(fmt.Sprintf("  retired %s", pubDisplay))
			}
		} else if k.Active {
			line = fmt.Sprintf("%s %s %s", greenStyle.Render("*"), greenStyle.Render("current"), faintStyle.Render(pubDisplay))
		} else {
			line = fmt.Sprintf("  %s %s", faintStyle.Render("retired"), faintStyle.Render(pubDisplay))
		}
		allLines = append(allLines, line)
	}
	visible, offset := clipLines(allLines, maxLines, m.selected[panelKeys])
	return scrollIndicators(visible, offset, len(allLines), maxLines)
}

func (m model) renderRemotesList(maxLines int) string {
	if len(m.remotes) == 0 {
		return faintStyle.Render("No remotes configured")
	}
	var allLines []string

	allLines = append(allLines, faintStyle.Render("-- plaintext repo --"))
	hasPlaintext := false
	for i, r := range m.remotes {
		if r.RepoType != "plaintext" {
			continue
		}
		hasPlaintext = true
		if m.activePanel == panelRemotes && i == m.selected[panelRemotes] {
			allLines = append(allLines, selectedStyle.Render(fmt.Sprintf("  %s: %s", r.Name, r.URL)))
		} else {
			allLines = append(allLines, fmt.Sprintf("  %s: %s", blueStyle.Render(r.Name), r.URL))
		}
	}
	if !hasPlaintext {
		allLines = append(allLines, faintStyle.Render("  (none)"))
	}

	allLines = append(allLines, faintStyle.Render("-- encrypted repo --"))
	hasEncrypted := false
	for i, r := range m.remotes {
		if r.RepoType != "encrypted" {
			continue
		}
		hasEncrypted = true
		if m.activePanel == panelRemotes && i == m.selected[panelRemotes] {
			allLines = append(allLines, selectedStyle.Render(fmt.Sprintf("  %s: %s", r.Name, r.URL)))
		} else {
			allLines = append(allLines, fmt.Sprintf("  %s: %s", greenStyle.Render(r.Name), r.URL))
		}
	}
	if !hasEncrypted {
		allLines = append(allLines, faintStyle.Render("  (none -- press [e])"))
	}

	visible, offset := clipLines(allLines, maxLines, m.selected[panelRemotes])
	return scrollIndicators(visible, offset, len(allLines), maxLines)
}

func (m model) renderCommandLog(maxLines, maxW int) string {
	if len(m.commandLog) == 0 {
		return faintStyle.Render("No commands yet")
	}
	var lines []string
	for _, entry := range m.commandLog {
		display := entry
		if len(display) > maxW {
			display = display[:maxW-3] + "..."
		}
		if strings.HasPrefix(entry, "$ ") {
			lines = append(lines, faintStyle.Render(display))
		} else {
			lines = append(lines, display)
		}
	}
	visible := clipLinesTail(lines, maxLines)
	return strings.Join(visible, "\n")
}

func (m model) renderFooter() string {
	if m.statusMsg != "" {
		if strings.Contains(m.statusMsg, "failed") || strings.Contains(m.statusMsg, "Failed") {
			return redStyle.Render(m.statusMsg)
		}
		successKeywords := []string{"successfully", "Synced", "Initialized", "Pushed", "Decrypted", "Added encrypted"}
		for _, kw := range successKeywords {
			if strings.Contains(m.statusMsg, kw) {
				return greenStyle.Render(m.statusMsg)
			}
		}
		return yellowStyle.Render(m.statusMsg)
	}
	keys := []string{
		cyanStyle.Render("[i]") + "nit",
		cyanStyle.Render("[s]") + "ync",
		cyanStyle.Render("[p]") + "ush",
		cyanStyle.Render("[e]") + " remote",
		cyanStyle.Render("[D]") + "ecrypt",
		cyanStyle.Render("[R]") + "ekey",
		cyanStyle.Render("[r]") + "eload",
		cyanStyle.Render("[tab]") + "panel",
		cyanStyle.Render("[?]") + "help",
		cyanStyle.Render("[q]") + "uit",
	}
	return strings.Join(keys, "  ")
}

func (m model) renderNotInitialized() string {
	repoName := filepath.Base(m.prereqs.RepoPath)
	header := headerStyle.Render("lazycrypt") +
		faintStyle.Render(" --- ") +
		boldStyle.Render(fmt.Sprintf("repo: %s", repoName)) +
		faintStyle.Render("  ") +
		cyanStyle.Render(m.prereqs.CurrentBranch) +
		faintStyle.Render(" --- ") +
		redStyle.Render("not initialized")

	var body strings.Builder
	body.WriteString("\n  lazycrypt is not initialized in this repository.\n\n")
	body.WriteString(fmt.Sprintf("  Press %s to initialize:\n", cyanStyle.Render("[i]")))
	body.WriteString("    - Creates .lazycrypt/ directory\n")
	body.WriteString("    - Generates age encryption keypair\n")
	body.WriteString("    - Initializes encrypted mirror repo\n\n")
	body.WriteString("  Prerequisites:\n")

	prereqLine := func(ok bool, label, failMsg string) string {
		if ok {
			return fmt.Sprintf("    %s %s\n", greenStyle.Render("v"), label)
		}
		return fmt.Sprintf("    %s %s\n", redStyle.Render("x"), failMsg)
	}
	body.WriteString(prereqLine(m.prereqs.HasGit, "git installed", "git not found"))
	body.WriteString(prereqLine(m.prereqs.HasAge, "age installed", "age not found (brew install age)"))
	body.WriteString(prereqLine(m.prereqs.InsideRepo, "git repo detected", "not in a git repo"))

	footer := "\n" + faintStyle.Render("[i]nit [?]help [q]uit")
	if m.statusMsg != "" {
		if strings.Contains(m.statusMsg, "failed") {
			footer = "\n" + redStyle.Render(m.statusMsg)
		} else {
			footer = "\n" + greenStyle.Render(m.statusMsg)
		}
	}

	return header + "\n" + body.String() + footer
}

func (m model) renderHelp() string {
	var b strings.Builder
	b.WriteString(headerStyle.Render("lazycrypt - Dual History Encryption") + "\n\n")
	b.WriteString(boldStyle.Render("  Sender workflow:") + "\n")
	b.WriteString(fmt.Sprintf("    %s  Initialize lazycrypt in current repo\n", cyanStyle.Render("i")))
	b.WriteString(fmt.Sprintf("    %s  Sync plaintext commits to encrypted mirror\n", cyanStyle.Render("s")))
	b.WriteString(fmt.Sprintf("    %s  Push encrypted repo to remote (force)\n", cyanStyle.Render("p")))
	b.WriteString(fmt.Sprintf("    %s  Configure encrypted remote (name + URL)\n", cyanStyle.Render("e")))
	b.WriteString(fmt.Sprintf("    %s  Re-key (press twice to confirm)\n", cyanStyle.Render("R")))
	b.WriteString("\n")
	b.WriteString(boldStyle.Render("  Receiver workflow:") + "\n")
	b.WriteString(fmt.Sprintf("    %s  Pull encrypted repo & decrypt into plaintext\n", cyanStyle.Render("D")))
	b.WriteString("\n")
	b.WriteString(boldStyle.Render("  Navigation:") + "\n")
	b.WriteString(fmt.Sprintf("    %s      Reload data\n", cyanStyle.Render("r")))
	b.WriteString(fmt.Sprintf("    %s    Switch active panel\n", cyanStyle.Render("tab")))
	b.WriteString(fmt.Sprintf("    %s    Navigate / scroll diff\n", cyanStyle.Render("j/k")))
	b.WriteString(fmt.Sprintf("    %s      Toggle this help\n", cyanStyle.Render("?")))
	b.WriteString(fmt.Sprintf("    %s      Quit\n\n", cyanStyle.Render("q")))
	b.WriteString(faintStyle.Render("  Press ? or Esc to close"))
	return b.String()
}

// --- Main ---

func main() {
	p := tea.NewProgram(initialModel(), tea.WithAltScreen())
	if _, err := p.Run(); err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}
}
