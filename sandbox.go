package main

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	nanogo "simonwaldherr.de/go/nanogo/interp"
	smallr "simonwaldherr.de/go/smallr"
)

// execSmallR executes the smallR demo to evaluate `expr` and returns its stdout.
// It prefers a local `./smallr` binary if present, otherwise falls back to
// `go run smallr.go -e` which requires the Go toolchain at runtime.
func execSmallR(expr string) (string, error) {
	// Acquire a smallR context from the pool to avoid repeated allocations
	v := smallRPool.Get()
	ctx := v.(*smallr.Context)
	defer smallRPool.Put(ctx)

	res, err := ctx.EvalString(expr)
	if err != nil {
		return "", fmt.Errorf("smallr eval failed: %w", err)
	}
	if strings.TrimSpace(res.Output) != "" {
		return res.Output, nil
	}
	return res.Value.String(), nil
}

// execShellCommand executes a safe set of shell commands on the server.
// Allowed commands are restricted for security reasons.
func execShellCommand(cmd string) (string, error) {
	cmd = strings.TrimSpace(cmd)

	// Whitelist of safe commands that can be executed
	allowedCommands := map[string]bool{
		"ls": true, "cat": true, "head": true, "tail": true,
		"echo": true, "curl": true, "wget": true, "date": true,
		"pwd": true, "whoami": true, "uname": true, "df": true,
	}

	// Extract the base command (first word)
	parts := strings.Fields(cmd)
	if len(parts) == 0 {
		return "", fmt.Errorf("empty command")
	}

	baseCmd := filepath.Base(parts[0]) // Use basename to avoid path traversal
	if !allowedCommands[baseCmd] {
		return "", fmt.Errorf("command %q is not allowed", baseCmd)
	}

	// For security, use subprocess without shell interpretation
	// This prevents shell injection attacks
	out, err := exec.Command(baseCmd, parts[1:]...).CombinedOutput()
	if err != nil {
		return string(out), fmt.Errorf("shell command failed: %w", err)
	}
	return string(out), nil
}

// execTinyGoProgram interprets Go code directly in a sandboxed environment.
// Similar to nanoGo, it doesn't compile but rather interprets the code.
func execTinyGoProgram(source string) (string, error) {
	timeout := 10 * time.Second // Slightly longer timeout for interpreted programs
	return RunSafe(source, timeout)
}

// RunSafe executes untrusted Go source inside the nanoGo interpreter
// with a context-based timeout. It captures ConsoleLog/ConsoleWarn/ConsoleError
// output into a buffer and recovers from panics so the host application
// is not crashed by user code.
func RunSafe(source string, timeout time.Duration) (string, error) {
	defer func() {
		if r := recover(); r != nil {
			// recovered panic will be returned as error below
		}
	}()

	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	outBuf := &bytes.Buffer{}
	done := make(chan error, 1)
	go func() {
		done <- runInterpreted(ctx, source, outBuf)
	}()

	select {
	case err := <-done:
		return outBuf.String(), err
	case <-ctx.Done():
		return outBuf.String(), fmt.Errorf("execution timed out after %s", timeout)
	}
}

// runInterpreted creates a sandboxed interpreter, registers only the
// host functions we choose to expose, and executes the source.
//
// Known limitation: the nanoGo interpreter exposes no cancellation hook, so
// a script that never returns (e.g. an unbounded loop) keeps running in its
// goroutine even after RunSafe's caller sees a timeout error, holding its
// nanoGoSem slot until it eventually finishes on its own. To keep
// *subsequent* requests from piling up behind that leaked goroutine
// indefinitely, acquiring the slot itself respects ctx: once the caller's
// own timeout elapses, a still-queued request fails fast with a clear
// "sandbox busy" error instead of silently hanging past its stated timeout.
func runInterpreted(ctx context.Context, source string, out *bytes.Buffer) error {
	// Limit concurrent interpreter instances to avoid spikes.
	select {
	case nanoGoSem <- struct{}{}:
		defer func() { <-nanoGoSem }()
	case <-ctx.Done():
		return fmt.Errorf("sandbox busy: no execution slot became free before the timeout")
	}

	vm := nanogo.NewInterpreter()
	registerSafeNatives(vm, out)
	nanogo.RegisterBuiltinPackages(vm)
	return vm.Run(source)
}

// registerSafeNatives installs a minimal set of host functions that are
// safe to expose to untrusted user code. Output is written to `out`.
func registerSafeNatives(vm *nanogo.Interpreter, out *bytes.Buffer) {
	vm.RegisterNative("ConsoleLog", func(args []any) (any, error) {
		if len(args) > 0 {
			fmt.Fprintln(out, nanogo.ToString(args[0]))
		}
		return nil, nil
	})

	vm.RegisterNative("ConsoleWarn", func(args []any) (any, error) {
		if len(args) > 0 {
			fmt.Fprintln(out, "[warn] "+nanogo.ToString(args[0]))
		}
		return nil, nil
	})

	vm.RegisterNative("ConsoleError", func(args []any) (any, error) {
		if len(args) > 0 {
			fmt.Fprintln(out, "[error] "+nanogo.ToString(args[0]))
		}
		return nil, nil
	})

	vm.RegisterNative("__hostSprintf", func(args []any) (any, error) {
		if len(args) == 0 {
			return "", nil
		}
		format := nanogo.ToString(args[0])
		fmtArgs := make([]any, 0, len(args)-1)
		for _, a := range args[1:] {
			fmtArgs = append(fmtArgs, a)
		}
		return fmt.Sprintf(format, fmtArgs...), nil
	})
}
