package logx

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// newBufferHandler returns a text handler at level, plus the buffer it writes
// to, so a test can say what actually reached that child.
func newBufferHandler(level slog.Level) (slog.Handler, *bytes.Buffer) {
	buf := &bytes.Buffer{}
	return slog.NewTextHandler(buf, &slog.HandlerOptions{Level: level}), buf
}

// stubHandler is a child a test can watch: it reports one fixed level, counts
// the records it is asked to handle, and can be made to fail.
type stubHandler struct {
	level   slog.Level
	wantErr error
	calls   int
}

func (h *stubHandler) Enabled(_ context.Context, level slog.Level) bool { return level >= h.level }

func (h *stubHandler) Handle(context.Context, slog.Record) error {
	h.calls++
	return h.wantErr
}

func (h *stubHandler) WithAttrs([]slog.Attr) slog.Handler { return h }

func (h *stubHandler) WithGroup(string) slog.Handler { return h }

func testRecord(level slog.Level, message string) slog.Record {
	return slog.NewRecord(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC), level, message, 0)
}

// TestMultiHandlerRoutesByLevel proves the fan-out the error log file depends
// on: every record reaches the console handler, but only records at or above
// its level reach the error-file handler.
func TestMultiHandlerRoutesByLevel(t *testing.T) {
	consoleHandler, console := newBufferHandler(slog.LevelInfo)
	fileHandler, file := newBufferHandler(slog.LevelError)
	logger := slog.New(NewMultiHandler(consoleHandler, fileHandler))

	logger.Info("hello")
	logger.Error("boom")

	if got := console.String(); !strings.Contains(got, "msg=hello") || !strings.Contains(got, "msg=boom") {
		t.Errorf("console should carry the info and the error record, got:\n%s", got)
	}
	got := file.String()
	if !strings.Contains(got, "msg=boom") {
		t.Errorf("file should carry the error record, got:\n%s", got)
	}
	if strings.Contains(got, "msg=hello") {
		t.Errorf("file should not carry the info record, got:\n%s", got)
	}
}

// TestMultiHandlerEnabledIfAnyChildEnabled proves Enabled reports true when
// any child is enabled at the level, and false when none is.
func TestMultiHandlerEnabledIfAnyChildEnabled(t *testing.T) {
	consoleHandler, _ := newBufferHandler(slog.LevelInfo)
	fileHandler, _ := newBufferHandler(slog.LevelError)
	multi := NewMultiHandler(consoleHandler, fileHandler)

	ctx := context.Background()
	if multi.Enabled(ctx, slog.LevelDebug) {
		t.Error("Enabled(debug) should be false: no child is enabled at debug")
	}
	if !multi.Enabled(ctx, slog.LevelInfo) {
		t.Error("Enabled(info) should be true: the console child is enabled at info")
	}

	empty := NewMultiHandler()
	for _, level := range []slog.Level{slog.LevelDebug, slog.LevelInfo, slog.LevelWarn, slog.LevelError} {
		if empty.Enabled(ctx, level) {
			t.Errorf("Enabled(%v) should be false: the handler has no children", level)
		}
	}
}

// TestMultiHandlerWithAttrsReachesAllChildren proves attributes added with
// With propagate to every child, not just the first.
func TestMultiHandlerWithAttrsReachesAllChildren(t *testing.T) {
	consoleHandler, console := newBufferHandler(slog.LevelInfo)
	fileHandler, file := newBufferHandler(slog.LevelError)

	slog.New(NewMultiHandler(consoleHandler, fileHandler)).With("service", "sync").Error("boom")

	for name, got := range map[string]string{"console": console.String(), "file": file.String()} {
		if !strings.Contains(got, "service=sync") {
			t.Errorf("%s should carry the attribute added with With, got:\n%s", name, got)
		}
	}
}

// TestMultiHandlerWithGroupReachesAllChildren proves a group opened with
// WithGroup propagates to every child.
func TestMultiHandlerWithGroupReachesAllChildren(t *testing.T) {
	consoleHandler, console := newBufferHandler(slog.LevelInfo)
	fileHandler, file := newBufferHandler(slog.LevelError)

	slog.New(NewMultiHandler(consoleHandler, fileHandler)).WithGroup("run").Error("boom", "step", 1)

	for name, got := range map[string]string{"console": console.String(), "file": file.String()} {
		if !strings.Contains(got, "run.step=1") {
			t.Errorf("%s should carry the grouped attribute, got:\n%s", name, got)
		}
	}
}

// TestMultiHandlerReturnsFirstChildError pins the current behaviour: Handle
// returns on the first child error, so a failing console handler stops the
// record from reaching the error-file handler. If the factory decides a
// failing child should not short-circuit the fan-out, that is a behaviour
// change to make in its own issue.
func TestMultiHandlerReturnsFirstChildError(t *testing.T) {
	wantErr := errors.New("first child failed")
	first := &stubHandler{level: slog.LevelInfo, wantErr: wantErr}
	second := &stubHandler{level: slog.LevelInfo}

	err := NewMultiHandler(first, second).Handle(context.Background(), testRecord(slog.LevelInfo, "boom"))

	if !errors.Is(err, wantErr) {
		t.Errorf("Handle error = %v; want %v", err, wantErr)
	}
	if first.calls != 1 {
		t.Errorf("first child calls = %d; want 1", first.calls)
	}
	if second.calls != 0 {
		t.Errorf("second child calls = %d; want 0: Handle stops at the first error", second.calls)
	}
}

// TestLoggerWritesErrorsToFileAndInfoToConsole assembles the logger exactly
// as cmd/snyk-dast-linear-sync's newLogger does -- a console TextHandler at
// Info and a file TextHandler at Error behind one MultiHandler -- and proves
// the error log file carries only errors while the console carries both.
func TestLoggerWritesErrorsToFileAndInfoToConsole(t *testing.T) {
	dir := t.TempDir()
	filePath := filepath.Join(dir, "error.log")

	logFile, err := os.Create(filePath)
	if err != nil {
		t.Fatalf("create temp file: %v", err)
	}
	defer logFile.Close()

	// The console buffer stands in for os.Stderr in newLogger.
	consoleBuf := &bytes.Buffer{}
	consoleHandler := slog.NewTextHandler(consoleBuf, &slog.HandlerOptions{Level: slog.LevelInfo})
	fileHandler := slog.NewTextHandler(logFile, &slog.HandlerOptions{Level: slog.LevelError})
	logger := slog.New(NewMultiHandler(consoleHandler, fileHandler))

	logger.Info("started")
	logger.Error("failed")

	if err := logFile.Sync(); err != nil {
		t.Fatalf("sync file: %v", err)
	}
	contents, err := os.ReadFile(filePath)
	if err != nil {
		t.Fatalf("read file: %v", err)
	}

	gotFile := string(contents)
	if !strings.Contains(gotFile, "msg=failed") {
		t.Errorf("error file should carry the error record, got:\n%s", gotFile)
	}
	if strings.Contains(gotFile, "msg=started") {
		t.Errorf("error file should not carry the info record, got:\n%s", gotFile)
	}

	gotConsole := consoleBuf.String()
	if !strings.Contains(gotConsole, "msg=started") || !strings.Contains(gotConsole, "msg=failed") {
		t.Errorf("console should carry both records, got:\n%s", gotConsole)
	}
}
