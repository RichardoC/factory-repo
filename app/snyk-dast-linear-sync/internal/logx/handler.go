package logx

import (
	"context"
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

func (h *MultiHandler) Handle(ctx context.Context, record slog.Record) error {
	for _, handler := range h.handlers {
		if !handler.Enabled(ctx, record.Level) {
			continue
		}
		if err := handler.Handle(ctx, record); err != nil {
			return err
		}
	}
	return nil
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
