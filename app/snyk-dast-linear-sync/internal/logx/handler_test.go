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

// TestMultiHandlerFailsIndependently pins the fan-out: a child that fails
// does not stop the record reaching its siblings, and its error is still
// reported. A failing console handler must not starve the error-file handler
// of the records that explain what happened.
func TestMultiHandlerFailsIndependently(t *testing.T) {
	wantErr := errors.New("console child failed")
	console := &stubHandler{level: slog.LevelInfo, wantErr: wantErr}
	file := &stubHandler{level: slog.LevelInfo}

	err := NewMultiHandler(console, file).Handle(context.Background(), testRecord(slog.LevelInfo, "boom"))

	if err == nil {
		t.Fatal("Handle error = nil; want the failing child's error")
	}
	if !errors.Is(err, wantErr) {
		t.Errorf("Handle error = %v; want it to mention %v", err, wantErr)
	}
	if !strings.Contains(err.Error(), wantErr.Error()) {
		t.Errorf("Handle error = %q; want it to mention %q", err, wantErr)
	}
	if console.calls != 1 {
		t.Errorf("console child calls = %d; want 1", console.calls)
	}
	if file.calls != 1 {
		t.Errorf("file child calls = %d; want 1: a failing sibling must not stop the fan-out", file.calls)
	}
}

// TestMultiHandlerReportsEveryChildError proves Handle joins the errors of
// every failing enabled child rather than returning only the first, and that
// disabled children are neither called nor reported.
func TestMultiHandlerReportsEveryChildError(t *testing.T) {
	consoleErr := errors.New("console child failed")
	fileErr := errors.New("file child failed")
	console := &stubHandler{level: slog.LevelInfo, wantErr: consoleErr}
	file := &stubHandler{level: slog.LevelError, wantErr: fileErr}
	disabled := &stubHandler{level: slog.LevelError + 1, wantErr: errors.New("must never be called")}

	err := NewMultiHandler(console, file, disabled).Handle(context.Background(), testRecord(slog.LevelError, "boom"))

	for _, wantErr := range []error{consoleErr, fileErr} {
		if !errors.Is(err, wantErr) {
			t.Errorf("Handle error = %v; want it to mention %v", err, wantErr)
		}
	}
	if disabled.calls != 0 {
		t.Errorf("disabled child calls = %d; want 0", disabled.calls)
	}
}

// TestMultiHandlerReturnsNilWhenEveryChildSucceeds proves a clean fan-out
// reports no error.
func TestMultiHandlerReturnsNilWhenEveryChildSucceeds(t *testing.T) {
	console, _ := newBufferHandler(slog.LevelInfo)
	file, _ := newBufferHandler(slog.LevelInfo)

	if err := NewMultiHandler(console, file).Handle(context.Background(), testRecord(slog.LevelInfo, "boom")); err != nil {
		t.Errorf("Handle error = %v; want nil", err)
	}
}

// TestMultiHandlerFailingConsoleStillReachesErrorFile assembles the logger
// exactly as cmd/snyk-dast-linear-sync's newLogger does, with the console
// child written to an always-failing writer, and proves the error record
// still reaches the error log file while the console failure is returned.
func TestMultiHandlerFailingConsoleStillReachesErrorFile(t *testing.T) {
	dir := t.TempDir()
	filePath := filepath.Join(dir, "error.log")

	logFile, err := os.Create(filePath)
	if err != nil {
		t.Fatalf("create temp file: %v", err)
	}
	defer logFile.Close()

	writeErr := errors.New("console write failed")
	consoleHandler := slog.NewTextHandler(failingWriter{err: writeErr}, &slog.HandlerOptions{Level: slog.LevelInfo})
	fileHandler := slog.NewTextHandler(logFile, &slog.HandlerOptions{Level: slog.LevelError})

	err = NewMultiHandler(consoleHandler, fileHandler).Handle(
		context.Background(), testRecord(slog.LevelError, "failed"))

	if err == nil {
		t.Fatal("Handle error = nil; want the console failure to be reported")
	}
	if !errors.Is(err, writeErr) {
		t.Errorf("Handle error = %v; want it to mention %v", err, writeErr)
	}

	if syncErr := logFile.Sync(); syncErr != nil {
		t.Fatalf("sync file: %v", syncErr)
	}
	contents, readErr := os.ReadFile(filePath)
	if readErr != nil {
		t.Fatalf("read file: %v", readErr)
	}
	if !strings.Contains(string(contents), "msg=failed") {
		t.Errorf("error file should carry the record despite the console failure, got:\n%s", contents)
	}
}

// failingWriter is a writer that always fails, standing in for a closed pipe
// or a full disk on the console side.
type failingWriter struct{ err error }

func (w failingWriter) Write([]byte) (int, error) { return 0, w.err }

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
