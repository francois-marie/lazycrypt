package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
)

// TestIntegrationSenderReceiver exercises the full sender → encrypt → push →
// pull → decrypt → verify workflow. It mutates the process cwd via os.Chdir
// (required by lazycrypt functions) and therefore MUST NOT run in parallel with
// other tests that depend on cwd.
func TestIntegrationSenderReceiver(t *testing.T) {
	// 1. Setup temporary directories
	tmpDir := t.TempDir()
	senderPath := filepath.Join(tmpDir, "sender")
	receiverPath := filepath.Join(tmpDir, "receiver")
	remotePath := filepath.Join(tmpDir, "remote.git")

	err := os.MkdirAll(senderPath, 0755)
	if err != nil {
		t.Fatalf("failed to create sender dir: %v", err)
	}
	err = os.MkdirAll(receiverPath, 0755)
	if err != nil {
		t.Fatalf("failed to create receiver dir: %v", err)
	}

	// 2. Initialize sender repo
	runGit(t, senderPath, "init")
	runGit(t, senderPath, "config", "user.name", "Test User")
	runGit(t, senderPath, "config", "user.email", "test@example.com")
	runGit(t, senderPath, "config", "commit.gpgsign", "false")

	// 3. Create complex commits in sender
	createFile(t, senderPath, "file1.txt", "Initial content")
	runGit(t, senderPath, "add", "file1.txt")
	runGit(t, senderPath, "commit", "-m", "Initial commit")
	initialSHA := getHeadSHA(t, senderPath)

	// Commit with symbols and long message
	longMessage := "Long message with symbols: !@#$%^&*()_+{}[]:\"|;'<>?,./ and emojis 🚀🔥\n\nMore details here.\nAnd even more lines."
	createFile(t, senderPath, "file2.txt", "Content with symbols and stuff")
	runGit(t, senderPath, "add", "file2.txt")
	runGit(t, senderPath, "commit", "-m", longMessage)
	secondSHA := getHeadSHA(t, senderPath)

	// Commit with files in subdirectories
	err = os.MkdirAll(filepath.Join(senderPath, "subdir"), 0755)
	if err != nil {
		t.Fatalf("failed to create subdir: %v", err)
	}
	createFile(t, senderPath, "subdir/deep.txt", "Deep content")
	runGit(t, senderPath, "add", "subdir/deep.txt")
	runGit(t, senderPath, "commit", "-m", "Subdir commit")
	thirdSHA := getHeadSHA(t, senderPath)

	// 4. Run lazycrypt init in sender
	chdir(t, senderPath)

	m := model{}
	initCmd := m.performInit()
	msg := initCmd()
	if finished, ok := msg.(initFinishedMsg); ok {
		if finished.err != nil {
			t.Fatalf("performInit failed: %v", finished.err)
		}
	} else {
		t.Fatalf("unexpected msg type from performInit: %T", msg)
	}

	// Compute absolute encrypted repo path while cwd is senderPath.
	absEncRepoPath, err := filepath.Abs(encryptedRepoPath())
	if err != nil {
		t.Fatalf("failed to resolve encrypted repo path: %v", err)
	}

	// 5. Sync commits
	// We call runSyncWithChannel directly and drain its messages.
	drainSync(t)

	// Check sender encrypted repo
	encLog := runGitOut(t, absEncRepoPath, "log", "--oneline", "--all")
	t.Logf("Sender encrypted repo log:\n%s", encLog)

	// 6. Setup local bare remote and push
	runGit(t, tmpDir, "init", "--bare", "remote.git")

	// Add encrypted remote to configuration
	cfg, err := loadConfig()
	if err != nil {
		t.Fatalf("failed to load config: %v", err)
	}
	cfg.EncryptedRemote = RemoteConfig{
		Name: "origin",
		URL:  remotePath,
	}
	err = saveConfig(cfg)
	if err != nil {
		t.Fatalf("failed to save config: %v", err)
	}

	// We also need to add it to the encrypted git repo's remotes so performPush works
	runGit(t, absEncRepoPath, "remote", "add", "origin", remotePath)

	pushCmd := m.performPush()
	msg = pushCmd()
	if finished, ok := msg.(pushFinishedMsg); ok {
		if finished.err != nil {
			t.Fatalf("performPush failed: %v", finished.err)
		}
	} else {
		t.Fatalf("unexpected msg type from performPush: %T", msg)
	}
	t.Logf("Push finished")

	// Check remote repo
	remoteLog := runGitOut(t, remotePath, "log", "--oneline", "--all")
	t.Logf("Remote repo log:\n%s", remoteLog)

	// 7. Initialize receiver repo and decrypt
	chdir(t, receiverPath)
	runGit(t, receiverPath, "init")
	runGit(t, receiverPath, "config", "user.name", "Test User")
	runGit(t, receiverPath, "config", "user.email", "test@example.com")
	runGit(t, receiverPath, "config", "commit.gpgsign", "false")

	// Key path from sender
	keyPath := filepath.Join(senderPath, ".lazycrypt", "keys", "current.key")

	// Initialize lazycrypt in receiver
	mRecv := model{}
	initCmd = mRecv.performInit()
	initCmd()

	// Set remote in receiver
	remoteCmd := mRecv.addEncryptedRemote("origin", remotePath)
	msg = remoteCmd()
	if finished, ok := msg.(remoteAddedMsg); ok {
		if finished.err != nil {
			t.Fatalf("addEncryptedRemote failed: %v", finished.err)
		}
	} else {
		t.Fatalf("unexpected msg type from addEncryptedRemote: %T", msg)
	}

	// Pull and decrypt
	decryptCmd := mRecv.performPullDecrypt(keyPath, receiverPath)
	msg = decryptCmd()
	if finished, ok := msg.(pullDecryptFinishedMsg); ok {
		if finished.err != nil {
			t.Fatalf("performPullDecrypt failed: %v", finished.err)
		}
		t.Logf("Pull & Decrypt finished: decrypted %d commits", finished.decryptedCount)
		if finished.decryptedCount != 3 {
			recvAbsEncRepo, _ := filepath.Abs(encryptedRepoPath())
			recvEncLog := runGitOut(t, recvAbsEncRepo, "log", "--oneline", "--all")
			t.Logf("Receiver encrypted repo log:\n%s", recvEncLog)
			t.Errorf("expected 3 decrypted commits, got %d", finished.decryptedCount)
		}
	} else {
		t.Fatalf("unexpected msg type from performPullDecrypt: %T", msg)
	}

	// 8. Verification
	// Check SHAs — both getHeadSHA and rev-list return full 40-char SHAs.
	gotInitialSHA := runGitOut(t, receiverPath, "rev-list", "--max-parents=0", "--max-count=1", "HEAD")
	if gotInitialSHA != initialSHA {
		t.Errorf("initial SHA mismatch: want %s, got %s", initialSHA, gotInitialSHA)
	}

	gotHeadSHA := getHeadSHA(t, receiverPath)
	if gotHeadSHA != thirdSHA {
		t.Errorf("head SHA mismatch: want %s, got %s", thirdSHA, gotHeadSHA)
	}

	// Check messages
	gotSecondMessage := runGitOut(t, receiverPath, "log", "--format=%B", "-n", "1", secondSHA)
	if strings.TrimSpace(gotSecondMessage) != strings.TrimSpace(longMessage) {
		t.Errorf("second commit message mismatch:\nwant: %q\ngot:  %q", longMessage, gotSecondMessage)
	}

	// Check file content
	checkFileContent(t, receiverPath, "file1.txt", "Initial content")
	checkFileContent(t, receiverPath, "file2.txt", "Content with symbols and stuff")
	checkFileContent(t, receiverPath, "subdir/deep.txt", "Deep content")

	fmt.Println("Integration test passed successfully!")
}

// --- Helpers ---

// chdir changes the process working directory and registers a t.Cleanup to
// restore the original cwd when the test finishes. This is required because
// lazycrypt functions use cwd-relative paths. Tests using chdir MUST NOT run
// in parallel.
func chdir(t *testing.T, dir string) {
	t.Helper()
	origWd, err := os.Getwd()
	if err != nil {
		t.Fatalf("failed to get cwd: %v", err)
	}
	if err := os.Chdir(dir); err != nil {
		t.Fatalf("failed to chdir to %s: %v", dir, err)
	}
	t.Cleanup(func() {
		os.Chdir(origWd)
	})
}

// drainSync runs the sync via runSyncWithChannel and drains all messages,
// logging progress along the way. Fails the test if sync returns an error.
func drainSync(t *testing.T) {
	t.Helper()
	ch := make(chan tea.Msg, 100)
	go runSyncWithChannel(ch)
	for msg := range ch {
		switch v := msg.(type) {
		case syncFinishedMsg:
			if v.err != nil {
				t.Fatalf("sync failed: %v", v.err)
			}
			t.Logf("Sync finished: synced %d commits", v.syncedCount)
			return
		case syncProgressMsg:
			t.Logf("Sync progress: %d/%d", v.Synced, v.Total)
		}
	}
}

func runGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v in %s failed: %v\nOutput: %s", args, dir, err, string(out))
	}
}

func runGitOut(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.Output()
	if err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			t.Fatalf("git %v in %s failed: %v\nStderr: %s", args, dir, err, string(exitErr.Stderr))
		}
		t.Fatalf("git %v in %s failed: %v", args, dir, err)
	}
	return strings.TrimSpace(string(out))
}

func getHeadSHA(t *testing.T, dir string) string {
	t.Helper()
	return runGitOut(t, dir, "rev-parse", "HEAD")
}

func createFile(t *testing.T, dir, name, content string) {
	t.Helper()
	err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0644)
	if err != nil {
		t.Fatalf("failed to create file %s: %v", name, err)
	}
}

func checkFileContent(t *testing.T, dir, name, expected string) {
	t.Helper()
	content, err := os.ReadFile(filepath.Join(dir, name))
	if err != nil {
		t.Fatalf("failed to read file %s: %v", name, err)
	}
	if string(content) != expected {
		t.Errorf("file %s content mismatch: want %q, got %q", name, expected, string(content))
	}
}
