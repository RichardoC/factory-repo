package logx

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
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

func TestMultiHandlerWithAttrsReachesEveryChild(t *testing.T) {
	consoleHandler, console := newBufferHandler(slog.LevelInfo)
	fileHandler, file := newBufferHandler(slog.LevelError)

	slog.New(NewMultiHandler(consoleHandler, fileHandler)).With("service", "sync").Error("boom")

	for name, got := range map[string]string{"console": console.String(), "file": file.String()} {
		if !strings.Contains(got, "service=sync") {
			t.Errorf("%s should carry the attribute added with With, got:\n%s", name, got)
		}
	}
}

func TestMultiHandlerWithGroupReachesEveryChild(t *testing.T) {
	consoleHandler, console := newBufferHandler(slog.LevelInfo)
	fileHandler, file := newBufferHandler(slog.LevelError)

	slog.New(NewMultiHandler(consoleHandler, fileHandler)).WithGroup("run").Error("boom", "step", 1)

	for name, got := range map[string]string{"console": console.String(), "file": file.String()} {
		if !strings.Contains(got, "run.step=1") {
			t.Errorf("%s should carry the grouped attribute, got:\n%s", name, got)
		}
	}
}

func TestMultiHandlerEnabledIsAnyChild(t *testing.T) {
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

// A failing child must not stop the remaining children: Handle calls every
// enabled child and joins the failures, so a broken console handler cannot
// keep records from reaching the file.
func TestMultiHandlerHandleCallsEveryChildAndJoinsErrors(t *testing.T) {
	firstErr := errors.New("first child failed")
	secondErr := errors.New("second child failed")
	first := &stubHandler{level: slog.LevelInfo, wantErr: firstErr}
	second := &stubHandler{level: slog.LevelInfo, wantErr: secondErr}

	err := NewMultiHandler(first, second).Handle(context.Background(), testRecord(slog.LevelInfo, "boom"))

	if !errors.Is(err, firstErr) {
		t.Errorf("Handle error = %v; want it to wrap %v", err, firstErr)
	}
	if !errors.Is(err, secondErr) {
		t.Errorf("Handle error = %v; want it to wrap %v", err, secondErr)
	}
	if first.calls != 1 {
		t.Errorf("first child calls = %d; want 1", first.calls)
	}
	if second.calls != 1 {
		t.Errorf("second child calls = %d; want 1: a failing sibling must not stop the remaining children", second.calls)
	}

	// A failing console handler must still let the record reach the file.
	console := &stubHandler{level: slog.LevelInfo, wantErr: secondErr}
	fileHandler, file := newBufferHandler(slog.LevelInfo)
	err = NewMultiHandler(console, fileHandler).Handle(context.Background(), testRecord(slog.LevelInfo, "boom"))
	if !errors.Is(err, secondErr) {
		t.Errorf("Handle error = %v; want it to wrap %v", err, secondErr)
	}
	if console.calls != 1 {
		t.Errorf("console child calls = %d; want 1", console.calls)
	}
	if !strings.Contains(file.String(), "msg=boom") {
		t.Errorf("file child should still receive the record after the console child failed, got:\n%s", file.String())
	}
}

func TestMultiHandlerSkipsChildrenBelowTheirLevel(t *testing.T) {
	child := &stubHandler{level: slog.LevelError}

	slog.New(NewMultiHandler(child)).Info("hello")

	if child.calls != 0 {
		t.Errorf("child calls = %d; want 0: an info record must not reach a child at error level", child.calls)
	}
}
