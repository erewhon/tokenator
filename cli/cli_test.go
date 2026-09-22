package cli

import (
	"context"
	"errors"
	"io"
	"log"
	"os"
	"strings"
	"testing"
)

// captureStderr runs fn with os.Stderr (and the log package, which bound
// the original os.Stderr at init) redirected and returns what it wrote.
func captureStderr(t *testing.T, fn func()) string {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	old := os.Stderr
	os.Stderr = w
	log.SetOutput(w)
	defer func() {
		os.Stderr = old
		log.SetOutput(old)
	}()
	done := make(chan string)
	go func() {
		b, _ := io.ReadAll(r)
		done <- string(b)
	}()
	fn()
	w.Close()
	return <-done
}

func TestRunHelp(t *testing.T) {
	var err error
	out := captureStderr(t, func() {
		err = Run(context.Background(), []string{"--help"})
	})
	// Top-level help is a successful command (like `version`): nil, not
	// flag.ErrHelp. Subcommand -h returns flag.ErrHelp instead.
	if err != nil {
		t.Fatalf("Run(--help) = %v, want nil", err)
	}
	if !strings.HasPrefix(out, "usage: tokenator <command> [flags]") {
		t.Fatalf("help not printed; stderr = %q", out)
	}
	if !strings.Contains(out, "  report    usage rollups") {
		t.Fatalf("help missing command list; stderr = %q", out)
	}
}

func TestRunUnknownCommand(t *testing.T) {
	var err error
	out := captureStderr(t, func() {
		err = Run(context.Background(), []string{"frobnicate"})
	})
	if err == nil {
		t.Fatal("Run(frobnicate) = nil, want error")
	}
	if !strings.Contains(err.Error(), "frobnicate") {
		t.Fatalf("error %q does not name the bad command", err)
	}
	var ue *UsageError
	if !errors.As(err, &ue) {
		t.Fatalf("error %T is not a *UsageError", err)
	}
	if !strings.Contains(out, `unknown command "frobnicate"`) || !strings.Contains(out, "usage: tokenator") {
		t.Fatalf("expected message and usage on stderr, got %q", out)
	}
}
