package main

import (
	"log/slog"
	"os"
	"path/filepath"
	"testing"
)

func TestParseLogLevel(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want slog.Level
		ok   bool
	}{
		{"DEBUG", slog.LevelDebug, true},
		{"debug", slog.LevelDebug, true},
		{"INFO", slog.LevelInfo, true},
		{"", slog.LevelInfo, true},
		{"WARN", slog.LevelWarn, true},
		{"WARNING", slog.LevelWarn, true}, // the documented spelling
		{"ERROR", slog.LevelError, true},
		{" error ", slog.LevelError, true},
		{"VERBOSE", slog.LevelInfo, false},
	} {
		got, ok := parseLogLevel(tc.in)
		if got != tc.want || ok != tc.ok {
			t.Errorf("parseLogLevel(%q) = (%v, %v), want (%v, %v)", tc.in, got, ok, tc.want, tc.ok)
		}
	}
}

func TestOpenLogOutputCreatesFileAndDirectory(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "gateway.log")

	out, closeFn, err := openLogOutput(path)
	if err != nil {
		t.Fatalf("openLogOutput: %v", err)
	}
	defer closeFn()

	if out == os.Stderr {
		t.Fatal("a configured LOG_FILE must be written to, not just stderr")
	}
	if _, err := out.Write([]byte("hello\n")); err != nil {
		t.Fatalf("write: %v", err)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	if string(b) != "hello\n" {
		t.Errorf("log file = %q, want %q", b, "hello\n")
	}
}

func TestOpenLogOutputEmptyPathIsStderr(t *testing.T) {
	out, closeFn, err := openLogOutput("")
	defer closeFn()
	if err != nil || out != os.Stderr {
		t.Errorf("openLogOutput(\"\") = (%v, %v), want (stderr, nil)", out, err)
	}
}

// Under launchd, stderr is redirected into the same file LOG_FILE names.
// Writing to both would duplicate every line.
func TestOpenLogOutputSkipsFileThatIsStderr(t *testing.T) {
	path := filepath.Join(t.TempDir(), "gateway.log")
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()

	saved := os.Stderr
	os.Stderr = f
	t.Cleanup(func() { os.Stderr = saved })

	out, closeFn, err := openLogOutput(path)
	if err != nil {
		t.Fatalf("openLogOutput: %v", err)
	}
	defer closeFn()
	if out != os.Stderr {
		t.Error("LOG_FILE is the same file as stderr; lines would be written twice")
	}
}
