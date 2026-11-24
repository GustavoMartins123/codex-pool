// Package requestlog carries the clock and logger used for proxy request logs.
package requestlog

import (
	"context"
	"log"
	"log/slog"
	"time"

	"codex-pool-proxy/internal/logredact"
)

type Runtime struct {
	Now    func() time.Time
	Logger *slog.Logger
}

func System() Runtime {
	return Runtime{
		Now:    time.Now,
		Logger: slog.New(logredact.NewTextHandler(log.Writer())),
	}
}

func (rt Runtime) LogRequest(ctx context.Context, requestID, message string, attrs ...slog.Attr) {
	handler := rt.Logger.Handler()
	if !handler.Enabled(ctx, slog.LevelInfo) {
		return
	}
	record := slog.NewRecord(rt.Now(), slog.LevelInfo, message, 0)
	record.AddAttrs(slog.String("request_id", requestID))
	record.AddAttrs(attrs...)
	_ = handler.Handle(ctx, record)
}
