package benchmarks

import (
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"slices"
	"strings"
	"testing"
)

// TestMain prints which code a run measured as benchmark configuration lines,
// which benchstat keeps with the results. go.mod replaces picatz/websocket
// with the parent directory, so its pseudo-version says nothing about the
// code under test: the commit of the checkout does, marked "-dirty" when it
// has uncommitted changes.
func TestMain(m *testing.M) {
	fmt.Printf("picatz-commit: %s\n", checkoutCommit())
	fmt.Printf("go: %s\n", runtime.Version())
	// Test binaries carry no module versions, so read them from go.mod.
	if mod, err := os.ReadFile("go.mod"); err == nil {
		for line := range strings.Lines(string(mod)) {
			fields := strings.Fields(line)
			if len(fields) == 2 && slices.Contains(compared, fields[0]) {
				fmt.Printf("%s: %s\n", strings.TrimPrefix(fields[0], "github.com/"), fields[1])
			}
		}
	}
	os.Exit(m.Run())
}

// compared lists the modules of the other libraries being compared.
var compared = []string{"github.com/coder/websocket", "github.com/gorilla/websocket", "github.com/gobwas/ws"}

// checkoutCommit returns the commit of the repository checkout, or "unknown"
// when git is unavailable.
func checkoutCommit() string {
	out, err := exec.Command("git", "rev-parse", "HEAD").Output()
	if err != nil {
		return "unknown"
	}
	commit := strings.TrimSpace(string(out))
	if status, err := exec.Command("git", "status", "--porcelain", "--", "..").Output(); err == nil && len(status) > 0 {
		commit += "-dirty"
	}
	return commit
}
