package app

import (
	"strings"
	"testing"
	"time"
)

func TestExecShellCommandRejectsDisallowed(t *testing.T) {
	if _, err := execShellCommand("rm -rf /"); err == nil {
		t.Fatal("expected rm to be rejected by the whitelist")
	}
	if _, err := execShellCommand("python3 -c 'evil()'"); err == nil {
		t.Fatal("expected python3 to be rejected by the whitelist")
	}
}

func TestExecShellCommandRejectsEmpty(t *testing.T) {
	if _, err := execShellCommand("   "); err == nil {
		t.Fatal("expected empty command to be rejected")
	}
}

func TestExecShellCommandStripsPathTraversalForDisallowedBinary(t *testing.T) {
	// filepath.Base strips any directory prefix before the whitelist check;
	// "rm" is not whitelisted regardless of what path it was smuggled through.
	_, err := execShellCommand("../../usr/bin/rm -rf /")
	if err == nil {
		t.Fatal("expected disallowed basename to be rejected even with a path prefix")
	}
	if !strings.Contains(err.Error(), "not allowed") {
		t.Errorf("expected a 'not allowed' error, got: %v", err)
	}
}

func TestRunSafeExecutesSimpleProgram(t *testing.T) {
	src := `
package main
import "fmt"
func main() {
	fmt.Println("hello from sandbox")
}
`
	out, err := RunSafe(src, 5*time.Second)
	if err != nil {
		t.Fatalf("RunSafe failed: %v", err)
	}
	if !strings.Contains(out, "hello from sandbox") {
		t.Errorf("expected program output to be captured, got %q", out)
	}
}

func TestRunSafeArithmetic(t *testing.T) {
	src := `
package main
import "fmt"
func main() {
	x := 6
	y := 7
	fmt.Println(x * y)
}
`
	out, err := RunSafe(src, 5*time.Second)
	if err != nil {
		t.Fatalf("RunSafe failed: %v", err)
	}
	if !strings.Contains(out, "42") {
		t.Errorf("expected '42' in output, got %q", out)
	}
}

func TestRunSafeInvalidSourceReturnsError(t *testing.T) {
	_, err := RunSafe("this is not valid go source {{{", 2*time.Second)
	if err == nil {
		t.Fatal("expected an error for invalid source")
	}
}

func TestRunSafeBoundsSourceAndTimeout(t *testing.T) {
	if _, err := RunSafe(strings.Repeat("x", maxNanoGoSourceRunes+1), time.Second); err == nil || !strings.Contains(err.Error(), "exceeds") {
		t.Fatalf("expected source-size rejection, got %v", err)
	}
	// The configured maximum is applied inside RunSafe rather than trusting
	// an unbounded timeout supplied by a caller.
	if maxNanoGoTimeout != 15*time.Second {
		t.Fatalf("unexpected nanoGo timeout ceiling: %s", maxNanoGoTimeout)
	}
}

func TestRunSafeTimesOutOnLongLoop(t *testing.T) {
	// nanoGo has no cancellation hook (see runInterpreted's doc comment): the
	// interpreter goroutine keeps running in the background after RunSafe
	// returns the timeout below, until this loop actually finishes on its
	// own. Keep the iteration count small enough that it drains in well
	// under a second, so it doesn't starve the shared nanoGoSem slot for
	// other tests running after this one.
	src := `
package main
func main() {
	sum := 0
	for i := 0; i < 3000000; i++ {
		sum = sum + i
	}
}
`
	_, err := RunSafe(src, 5*time.Millisecond)
	if err == nil {
		t.Fatal("expected a timeout error for a long-running loop")
	}
	if !strings.Contains(err.Error(), "timed out") {
		t.Errorf("expected a 'timed out' error, got: %v", err)
	}
	// Give the leaked background goroutine time to drain before the next
	// test tries to acquire the single nanoGoSem slot.
	time.Sleep(300 * time.Millisecond)
}

func TestRunInterpretedFailsFastWhenSandboxBusy(t *testing.T) {
	// Occupy the single nanoGoSem slot for the duration of this test.
	nanoGoSem <- struct{}{}
	defer func() { <-nanoGoSem }()

	_, err := RunSafe(`package main
func main() {}`, 30*time.Millisecond)
	if err == nil {
		t.Fatal("expected an error when the sandbox slot is unavailable")
	}
}

func TestExecTinyGoProgramDelegatesToRunSafe(t *testing.T) {
	out, err := execTinyGoProgram(`
package main
import "fmt"
func main() { fmt.Println("tinygo ok") }
`)
	if err != nil {
		t.Fatalf("execTinyGoProgram failed: %v", err)
	}
	if !strings.Contains(out, "tinygo ok") {
		t.Errorf("expected output, got %q", out)
	}
}

func TestExecSmallR(t *testing.T) {
	out, err := execSmallR("1 + 2")
	if err != nil {
		t.Fatalf("execSmallR failed: %v", err)
	}
	if !strings.Contains(out, "3") {
		t.Errorf("expected result to contain '3', got %q", out)
	}
}

func TestExecSmallRInvalidExpr(t *testing.T) {
	if _, err := execSmallR("((("); err == nil {
		t.Fatal("expected an error for invalid smallR expression")
	}
}
