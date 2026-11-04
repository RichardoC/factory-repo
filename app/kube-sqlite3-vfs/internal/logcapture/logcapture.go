// Package logcapture provides a slog handler that records every log record
// it receives, so a test can assert on one specific record — its level and
// its attributes — instead of on substrings of the whole log, which other
// log lines (for example the VFS's own sector errors) can satisfy by
// accident.
package logcapture

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"strings"
	"sync"
)

// Record is one captured slog record.
type Record struct {
	Level   slog.Level
	Message string
	Attrs   []slog.Attr
	// Text renders the record as "<level> <message> <k>=<v> ...", for
	// failure output only.
	Text string
}

// Capture is a slog.Handler that keeps every record routed through it.
type Capture struct {
	mu      sync.Mutex
	records []Record
}

// NewCapture returns a logger backed by a fresh Capture.
func NewCapture() (*slog.Logger, *Capture) {
	c := &Capture{}
	return slog.New(c), c
}

// Enabled accepts every level: a test wants everything its subject logs.
func (c *Capture) Enabled(context.Context, slog.Level) bool { return true }

// Handle stores the record.
func (c *Capture) Handle(_ context.Context, r slog.Record) error {
	var b bytes.Buffer
	fmt.Fprintf(&b, "%s %s", r.Level, r.Message)
	var attrs []slog.Attr
	r.Attrs(func(a slog.Attr) bool {
		attrs = append(attrs, a)
		fmt.Fprintf(&b, " %s=%v", a.Key, a.Value)
		return true
	})
	c.mu.Lock()
	defer c.mu.Unlock()
	c.records = append(c.records, Record{Level: r.Level, Message: r.Message, Attrs: attrs, Text: strings.TrimSpace(b.String())})
	return nil
}

// WithAttrs returns the capture unchanged: the tests log without preset
// attributes, and a shared mutable handler must not accumulate them.
func (c *Capture) WithAttrs([]slog.Attr) slog.Handler { return c }

// WithGroup returns the capture unchanged, for the same reason.
func (c *Capture) WithGroup(string) slog.Handler { return c }

// Find returns the first captured record whose message contains substr.
func (c *Capture) Find(substr string) (Record, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, r := range c.records {
		if strings.Contains(r.Message, substr) {
			return r, true
		}
	}
	return Record{}, false
}

// Text renders every captured record, one per line, for failure output.
func (c *Capture) Text() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	lines := make([]string, len(c.records))
	for i, r := range c.records {
		lines[i] = r.Text
	}
	if len(lines) == 0 {
		return "(no log records captured)"
	}
	return strings.Join(lines, "\n")
}

// Attr returns the string value of the record's attribute key, or "".
func (r Record) Attr(key string) string {
	for _, a := range r.Attrs {
		if a.Key == key {
			return a.Value.String()
		}
	}
	return ""
}
