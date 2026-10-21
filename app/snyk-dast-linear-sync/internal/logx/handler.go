package logx

import (
	"context"
	"errors"
	"log/slog"
)

type MultiHandler struct {
	handlers []slog.Handler
}

func NewMultiHandler(handlers ...slog.Handler) *MultiHandler {
	return &MultiHandler{handlers: handlers}
}

func (h *MultiHandler) Enabled(ctx context.Context, level slog.Level) bool {
	for _, handler := range h.handlers {
		if handler.Enabled(ctx, level) {
			return true
		}
	}
	return false
}

// Handle fans the record out to every enabled child and returns the joined
// error. It deliberately does not stop at the first child error: the console
// handler and the error-file handler share one MultiHandler, and a console
// write failure (a closed pipe, a full disk) must not starve the error log of
// the very records that explain what happened. A failing child never prevents
// the others from seeing the record.
func (h *MultiHandler) Handle(ctx context.Context, record slog.Record) error {
	var errs []error
	for _, handler := range h.handlers {
		if !handler.Enabled(ctx, record.Level) {
			continue
		}
		if err := handler.Handle(ctx, record); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

func (h *MultiHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	children := make([]slog.Handler, 0, len(h.handlers))
	for _, handler := range h.handlers {
		children = append(children, handler.WithAttrs(attrs))
	}
	return &MultiHandler{handlers: children}
}

func (h *MultiHandler) WithGroup(name string) slog.Handler {
	children := make([]slog.Handler, 0, len(h.handlers))
	for _, handler := range h.handlers {
		children = append(children, handler.WithGroup(name))
	}
	return &MultiHandler{handlers: children}
}
