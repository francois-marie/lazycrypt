package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// --- Pure function tests (no external dependencies) ---

func TestTruncate(t *testing.T) {
	tests := []struct {
		input    string
		max      int
		expected string
	}{
		{"hello", 10, "hello"},
		{"hello world", 8, "hello..."},
		{"hi", 2, "hi"},
		{"exactly", 7, "exactly"},
		{"abc", 3, "abc"},
		{"abcd", 4, "abcd"},
		{"abcde", 4, "a..."},
	}
	for _, tt := range tests {
		result := truncate(tt.input, tt.max)
		if result != tt.expected {
			t.Errorf("truncate(%q, %d) = %q, want %q", tt.input, tt.max, result, tt.expected)
		}
	}
}

func TestSplitNonEmpty(t *testing.T) {
	tests := []struct {
		input    string
		expected []string
	}{
		{"", nil},
		{"\n\n", nil},
		{"a\nb\nc", []string{"a", "b", "c"}},
		{"a\n\nb\n", []string{"a", "b"}},
		{"single", []string{"single"}},
	}
	for _, tt := range tests {
		result := splitNonEmpty(tt.input)
		if len(result) != len(tt.expected) {
			t.Errorf("splitNonEmpty(%q) length = %d, want %d", tt.input, len(result), len(tt.expected))
			continue
		}
		for i, v := range result {
			if v != tt.expected[i] {
				t.Errorf("splitNonEmpty(%q)[%d] = %q, want %q", tt.input, i, v, tt.expected[i])
			}
		}
	}
}

func TestExpandHome(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skip("cannot determine home directory")
	}
	tests := []struct {
		input    string
		expected string
	}{
		{"/absolute/path", "/absolute/path"},
		{"relative/path", "relative/path"},
		{"~/some/file", filepath.Join(home, "some/file")},
	}
	for _, tt := range tests {
		result := expandHome(tt.input)
		if result != tt.expected {
			t.Errorf("expandHome(%q) = %q, want %q", tt.input, result, tt.expected)
		}
	}
}

func TestMatchesExcludePattern(t *testing.T) {
	patterns := []string{"*.png", "*.jpg", "vendor/*"}
	tests := []struct {
		path     string
		expected bool
	}{
		{"image.png", true},
		{"dir/photo.jpg", true},
		{"main.go", false},
		{"vendor/lib.go", true},
		{"src/main.go", false},
		{"readme.txt", false},
		{"deep/nested/file.png", true},
	}
	for _, tt := range tests {
		result := matchesExcludePattern(tt.path, patterns)
		if result != tt.expected {
			t.Errorf("matchesExcludePattern(%q, patterns) = %v, want %v", tt.path, result, tt.expected)
		}
	}
}

func TestMatchesExcludePatternEmpty(t *testing.T) {
	if matchesExcludePattern("anything.go", nil) {
		t.Error("nil patterns should match nothing")
	}
	if matchesExcludePattern("anything.go", []string{}) {
		t.Error("empty patterns should match nothing")
	}
}

func TestParseCommitLines(t *testing.T) {
	input := "abc1234567890 Fix login bug\ndef5678901234 Add user model\n"
	commits := parseCommitLines(input, nil)
	if len(commits) != 2 {
		t.Fatalf("expected 2 commits, got %d", len(commits))
	}
	if commits[0].ShortSHA != "abc1234" {
		t.Errorf("expected ShortSHA abc1234, got %s", commits[0].ShortSHA)
	}
	if commits[0].Message != "Fix login bug" {
		t.Errorf("expected message 'Fix login bug', got %q", commits[0].Message)
	}
	if commits[1].Message != "Add user model" {
		t.Errorf("expected message 'Add user model', got %q", commits[1].Message)
	}
	if commits[0].Synced {
		t.Error("commits should not be synced without commit map")
	}
}

func TestParseCommitLinesWithMap(t *testing.T) {
	cm := &CommitMap{mapping: map[string]string{
		"abc1234567890": "enc123",
	}}
	input := "abc1234567890 Fix login bug\ndef5678901234 Add user model\n"
	commits := parseCommitLines(input, cm)
	if !commits[0].Synced {
		t.Error("first commit should be synced")
	}
	if commits[1].Synced {
		t.Error("second commit should not be synced")
	}
}

func TestParseCommitLinesEmpty(t *testing.T) {
	commits := parseCommitLines("", nil)
	if len(commits) != 0 {
		t.Errorf("expected 0 commits for empty input, got %d", len(commits))
	}
}

func TestReverseCommits(t *testing.T) {
	commits := []Commit{
		{SHA: "aaa", ShortSHA: "aaa", Message: "first"},
		{SHA: "bbb", ShortSHA: "bbb", Message: "second"},
		{SHA: "ccc", ShortSHA: "ccc", Message: "third"},
	}
	reverseCommits(commits)
	if commits[0].Message != "third" {
		t.Errorf("expected first element to be 'third', got %q", commits[0].Message)
	}
	if commits[2].Message != "first" {
		t.Errorf("expected last element to be 'first', got %q", commits[2].Message)
	}
}

func TestParseFileChanges(t *testing.T) {
	input := "A\tsrc/new.go\nM\tsrc/old.go\nD\tsrc/removed.go\n"
	// parseFileChanges uses strings.Fields which handles tabs
	files := parseFileChanges(input)
	if len(files) != 3 {
		t.Fatalf("expected 3 file changes, got %d", len(files))
	}
	if files[0].Status != "A" || files[0].Path != "src/new.go" {
		t.Errorf("unexpected file[0]: %+v", files[0])
	}
	if files[1].Status != "M" || files[1].Path != "src/old.go" {
		t.Errorf("unexpected file[1]: %+v", files[1])
	}
	if files[2].Status != "D" || files[2].Path != "src/removed.go" {
		t.Errorf("unexpected file[2]: %+v", files[2])
	}
}

func TestParseFileChangesEmpty(t *testing.T) {
	files := parseFileChanges("")
	if len(files) != 0 {
		t.Errorf("expected 0 files for empty input, got %d", len(files))
	}
}

// --- CommitMap tests ---

func TestCommitMapInMemory(t *testing.T) {
	cm := &CommitMap{mapping: make(map[string]string), filePath: ""}
	if cm.syncedCount() != 0 {
		t.Error("new commit map should have 0 synced")
	}
	if cm.isSynced("abc") {
		t.Error("abc should not be synced")
	}
	cm.add("abc", "enc_abc")
	if !cm.isSynced("abc") {
		t.Error("abc should be synced after add")
	}
	if cm.syncedCount() != 1 {
		t.Errorf("expected 1 synced, got %d", cm.syncedCount())
	}
}

func TestCommitMapPersistence(t *testing.T) {
	tmpDir := t.TempDir()
	mapPath := filepath.Join(tmpDir, "commit-map")

	cm := &CommitMap{mapping: make(map[string]string), filePath: mapPath}
	cm.add("sha1", "enc1")
	cm.add("sha2", "enc2")
	if err := cm.save(); err != nil {
		t.Fatalf("save failed: %v", err)
	}

	loaded := loadCommitMap(mapPath)
	if loaded.syncedCount() != 2 {
		t.Errorf("expected 2 synced after reload, got %d", loaded.syncedCount())
	}
	if !loaded.isSynced("sha1") {
		t.Error("sha1 should be synced after reload")
	}
	if !loaded.isSynced("sha2") {
		t.Error("sha2 should be synced after reload")
	}
}

func TestLoadCommitMapNonExistent(t *testing.T) {
	cm := loadCommitMap("/nonexistent/path/commit-map")
	if cm.syncedCount() != 0 {
		t.Error("loading non-existent commit map should return empty map")
	}
}

// --- Config tests ---

func TestConfigSaveLoad(t *testing.T) {
	tmpDir := t.TempDir()
	origDir, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	if err := os.Chdir(tmpDir); err != nil {
		t.Fatalf("chdir to tmpDir: %v", err)
	}
	defer func() {
		if err := os.Chdir(origDir); err != nil {
			t.Fatalf("chdir restore: %v", err)
		}
	}()

	if err := os.MkdirAll(lazycryptDir(), 0755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	cfg := Config{
		Version:    1,
		CurrentKey: ".lazycrypt/keys/current.key",
		EncryptedRemote: RemoteConfig{
			Name: "origin",
			URL:  "git@github.com:user/repo.git",
		},
		ExcludePatterns: []string{"*.png", "*.jpg"},
		RetiredKeys: []RetiredKey{
			{Path: ".lazycrypt/keys/retired-old.key", RetiredAt: "2025-01-01T00:00:00Z"},
		},
	}

	if err := saveConfig(cfg); err != nil {
		t.Fatalf("saveConfig failed: %v", err)
	}

	loaded, err := loadConfig()
	if err != nil {
		t.Fatalf("loadConfig failed: %v", err)
	}

	if loaded.Version != 1 {
		t.Errorf("expected version 1, got %d", loaded.Version)
	}
	if loaded.CurrentKey != cfg.CurrentKey {
		t.Errorf("expected key %q, got %q", cfg.CurrentKey, loaded.CurrentKey)
	}
	if loaded.EncryptedRemote.Name != "origin" {
		t.Errorf("expected remote name 'origin', got %q", loaded.EncryptedRemote.Name)
	}
	if loaded.EncryptedRemote.URL != cfg.EncryptedRemote.URL {
		t.Errorf("expected URL %q, got %q", cfg.EncryptedRemote.URL, loaded.EncryptedRemote.URL)
	}
	if len(loaded.ExcludePatterns) != 2 {
		t.Errorf("expected 2 exclude patterns, got %d", len(loaded.ExcludePatterns))
	}
	if len(loaded.RetiredKeys) != 1 {
		t.Errorf("expected 1 retired key, got %d", len(loaded.RetiredKeys))
	}
}

func TestLoadConfigNoFile(t *testing.T) {
	tmpDir := t.TempDir()
	origDir, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	if err := os.Chdir(tmpDir); err != nil {
		t.Fatalf("chdir to tmpDir: %v", err)
	}
	defer func() {
		if err := os.Chdir(origDir); err != nil {
			t.Fatalf("chdir restore: %v", err)
		}
	}()

	cfg, err := loadConfig()
	if err != nil {
		t.Fatalf("loadConfig with no file should not error: %v", err)
	}
	if cfg.Version != 1 {
		t.Errorf("default config should have version 1, got %d", cfg.Version)
	}
}

// --- addToGitignore tests ---

func TestAddToGitignore(t *testing.T) {
	tmpDir := t.TempDir()
	origDir, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	if err := os.Chdir(tmpDir); err != nil {
		t.Fatalf("chdir to tmpDir: %v", err)
	}
	defer func() {
		if err := os.Chdir(origDir); err != nil {
			t.Fatalf("chdir restore: %v", err)
		}
	}()

	if err := addToGitignore(".lazycrypt"); err != nil {
		t.Fatalf("addToGitignore failed: %v", err)
	}

	content, err := os.ReadFile(".gitignore")
	if err != nil {
		t.Fatalf("read .gitignore failed: %v", err)
	}
	if !strings.Contains(string(content), ".lazycrypt") {
		t.Error(".gitignore should contain .lazycrypt")
	}

	// Adding again should be idempotent
	if err := addToGitignore(".lazycrypt"); err != nil {
		t.Fatalf("second addToGitignore failed: %v", err)
	}
	content2, err := os.ReadFile(".gitignore")
	if err != nil {
		t.Fatalf("read .gitignore: %v", err)
	}
	count := strings.Count(string(content2), ".lazycrypt")
	if count != 1 {
		t.Errorf("expected .lazycrypt to appear once, appeared %d times", count)
	}
}

func TestAddToGitignoreExistingNoNewline(t *testing.T) {
	tmpDir := t.TempDir()
	origDir, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	if err := os.Chdir(tmpDir); err != nil {
		t.Fatalf("chdir to tmpDir: %v", err)
	}
	defer func() {
		if err := os.Chdir(origDir); err != nil {
			t.Fatalf("chdir restore: %v", err)
		}
	}()

	if err := os.WriteFile(".gitignore", []byte("node_modules"), 0644); err != nil {
		t.Fatalf("write .gitignore: %v", err)
	}
	if err := addToGitignore(".lazycrypt"); err != nil {
		t.Fatalf("addToGitignore failed: %v", err)
	}

	content, err := os.ReadFile(".gitignore")
	if err != nil {
		t.Fatalf("read .gitignore: %v", err)
	}
	if !strings.Contains(string(content), "node_modules\n.lazycrypt") {
		t.Errorf("expected newline before .lazycrypt, got: %q", string(content))
	}
}

// --- Path helper tests ---

func TestPathHelpers(t *testing.T) {
	if lazycryptDir() != ".lazycrypt" {
		t.Errorf("unexpected lazycryptDir: %s", lazycryptDir())
	}
	if configPath() != ".lazycrypt/config.yml" {
		t.Errorf("unexpected configPath: %s", configPath())
	}
	if commitMapPath() != ".lazycrypt/commit-map" {
		t.Errorf("unexpected commitMapPath: %s", commitMapPath())
	}
	if encryptedRepoPath() != ".lazycrypt/encrypted.git" {
		t.Errorf("unexpected encryptedRepoPath: %s", encryptedRepoPath())
	}
	if keysDir() != ".lazycrypt/keys" {
		t.Errorf("unexpected keysDir: %s", keysDir())
	}
	if currentKeyPath() != ".lazycrypt/keys/current.key" {
		t.Errorf("unexpected currentKeyPath: %s", currentKeyPath())
	}
	if recvCommitMapPath() != ".lazycrypt/recv-commit-map" {
		t.Errorf("unexpected recvCommitMapPath: %s", recvCommitMapPath())
	}
}

// --- Layout helper tests ---

func TestClipLines(t *testing.T) {
	lines := []string{"a", "b", "c", "d", "e"}
	visible, offset := clipLines(lines, 3, 2)
	if len(visible) != 3 {
		t.Errorf("expected 3 visible lines, got %d", len(visible))
	}
	if offset < 0 {
		t.Errorf("offset should be non-negative, got %d", offset)
	}
}

func TestClipLinesNoClipping(t *testing.T) {
	lines := []string{"a", "b"}
	visible, offset := clipLines(lines, 5, 0)
	if len(visible) != 2 {
		t.Errorf("expected 2 visible lines, got %d", len(visible))
	}
	if offset != 0 {
		t.Errorf("offset should be 0 when no clipping, got %d", offset)
	}
}

func TestClipLinesTail(t *testing.T) {
	lines := []string{"a", "b", "c", "d", "e"}
	visible := clipLinesTail(lines, 3)
	if len(visible) != 3 {
		t.Errorf("expected 3 visible lines, got %d", len(visible))
	}
	if visible[0] != "c" {
		t.Errorf("expected first visible to be 'c', got %q", visible[0])
	}
}

func TestClipLinesTailNoClipping(t *testing.T) {
	lines := []string{"a", "b"}
	visible := clipLinesTail(lines, 5)
	if len(visible) != 2 {
		t.Errorf("expected 2 visible lines, got %d", len(visible))
	}
}

// --- Panel item count ---

func TestPanelItemCount(t *testing.T) {
	m := model{
		activePanel:      panelPlaintext,
		plaintextCommits: []Commit{{}, {}},
		encryptedCommits: []Commit{{}, {}, {}},
		files:            []FileChange{{}, {}},
		keys:             []Key{{}},
		remotes:          []Remote{{}, {}},
	}
	if m.panelItemCount() != 2 {
		t.Errorf("plaintext panel should have 2 items, got %d", m.panelItemCount())
	}
	m.activePanel = panelEncrypted
	if m.panelItemCount() != 3 {
		t.Errorf("encrypted panel should have 3 items, got %d", m.panelItemCount())
	}
	m.activePanel = panelFiles
	if m.panelItemCount() != 2 {
		t.Errorf("files panel should have 2 items, got %d", m.panelItemCount())
	}
	m.activePanel = panelKeys
	if m.panelItemCount() != 1 {
		t.Errorf("keys panel should have 1 item, got %d", m.panelItemCount())
	}
	m.activePanel = panelRemotes
	if m.panelItemCount() != 2 {
		t.Errorf("remotes panel should have 2 items, got %d", m.panelItemCount())
	}
}

// --- Command log tests ---

func TestLogCmd(t *testing.T) {
	globalCmdLogMu.Lock()
	globalCmdLog = nil
	globalCmdLogMu.Unlock()

	logCmd("test %s", "message")
	entries := snapshotCmdLog()
	if len(entries) != 1 {
		t.Fatalf("expected 1 log entry, got %d", len(entries))
	}
	if entries[0] != "test message" {
		t.Errorf("expected 'test message', got %q", entries[0])
	}
}

func TestLogCmdOverflow(t *testing.T) {
	globalCmdLogMu.Lock()
	globalCmdLog = nil
	globalCmdLogMu.Unlock()

	for i := 0; i < 210; i++ {
		logCmd("entry %d", i)
	}
	entries := snapshotCmdLog()
	if len(entries) != 200 {
		t.Errorf("expected 200 entries after overflow, got %d", len(entries))
	}
}

// --- GetPublicKey tests ---

func TestGetPublicKey(t *testing.T) {
	tmpDir := t.TempDir()
	keyPath := filepath.Join(tmpDir, "test.key")

	content := `# created: 2025-01-01T00:00:00Z
# public key: age1qx3m7w2k8example
AGE-SECRET-KEY-1EXAMPLE`
	if err := os.WriteFile(keyPath, []byte(content), 0600); err != nil {
		t.Fatalf("write key file: %v", err)
	}

	pub := getPublicKey(keyPath)
	if pub != "age1qx3m7w2k8example" {
		t.Errorf("expected 'age1qx3m7w2k8example', got %q", pub)
	}
}

func TestGetPublicKeyNotFound(t *testing.T) {
	pub := getPublicKey("/nonexistent/key")
	if pub != "" {
		t.Errorf("expected empty string for missing key, got %q", pub)
	}
}

func TestGetPublicKeyNoComment(t *testing.T) {
	tmpDir := t.TempDir()
	keyPath := filepath.Join(tmpDir, "bad.key")
	if err := os.WriteFile(keyPath, []byte("AGE-SECRET-KEY-1EXAMPLE\n"), 0600); err != nil {
		t.Fatalf("write key file: %v", err)
	}

	pub := getPublicKey(keyPath)
	if pub != "" {
		t.Errorf("expected empty for key without public key comment, got %q", pub)
	}
}

// --- Model navigation tests ---

func TestMoveDownUp(t *testing.T) {
	m := model{
		activePanel:      panelEncrypted,
		encryptedCommits: []Commit{{SHA: "aaa0000"}, {SHA: "bbb0000"}, {SHA: "ccc0000"}},
	}
	if m.selected[panelEncrypted] != 0 {
		t.Fatal("should start at 0")
	}
	m.moveDown()
	if m.selected[panelEncrypted] != 1 {
		t.Errorf("expected 1 after moveDown, got %d", m.selected[panelEncrypted])
	}
	m.moveDown()
	if m.selected[panelEncrypted] != 2 {
		t.Errorf("expected 2 after second moveDown, got %d", m.selected[panelEncrypted])
	}
	m.moveDown()
	if m.selected[panelEncrypted] != 2 {
		t.Error("should not go past last item")
	}
	m.moveUp()
	if m.selected[panelEncrypted] != 1 {
		t.Errorf("expected 1 after moveUp, got %d", m.selected[panelEncrypted])
	}
	m.moveUp()
	m.moveUp()
	if m.selected[panelEncrypted] != 0 {
		t.Error("should not go below 0")
	}
}

// --- Scroll indicators test ---

func TestScrollIndicators(t *testing.T) {
	lines := []string{"a", "b", "c"}

	result := scrollIndicators(lines, 0, 3, 5)
	if !strings.Contains(result, "a") {
		t.Error("should contain all lines when no scrolling needed")
	}

	visible := []string{"b", "c", "d"}
	result = scrollIndicators(visible, 1, 6, 3)
	if !strings.Contains(result, "1 more") {
		t.Error("should show above indicator")
	}
	if !strings.Contains(result, "2 more") {
		t.Error("should show below indicator")
	}
}

// --- Key file permissions test ---

func TestKeyFilePerms(t *testing.T) {
	if keyFilePerms != 0600 {
		t.Errorf("keyFilePerms should be 0600, got %o", keyFilePerms)
	}
}
