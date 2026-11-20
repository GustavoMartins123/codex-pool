package main

import (
	"context"
	"log"
	"log/slog"
	"time"

	"codex-pool-proxy/internal/logredact"
)

type proxyRuntime struct {
	now    func() time.Time
	logger *slog.Logger
}

func systemProxyRuntime() proxyRuntime {
	return proxyRuntime{
		now:    time.Now,
		logger: slog.New(logredact.NewTextHandler(log.Writer())),
	}
}

func (rt proxyRuntime) logRequest(ctx context.Context, requestID, message string, attrs ...slog.Attr) {
	handler := rt.logger.Handler()
	if !handler.Enabled(ctx, slog.LevelInfo) {
		return
	}
	record := slog.NewRecord(rt.now(), slog.LevelInfo, message, 0)
	record.AddAttrs(slog.String("request_id", requestID))
	record.AddAttrs(attrs...)
	_ = handler.Handle(ctx, record)
}
