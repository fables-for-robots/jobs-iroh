package serve

import (
	"fmt"
	"log/slog"

	natsserver "github.com/nats-io/nats-server/v2/server"
)

// natsLogger adapts the embedded NATS server's logger interface onto slog.
type natsLogger struct{ log *slog.Logger }

func newNATSLogger(log *slog.Logger) natsserver.Logger { return &natsLogger{log: log} }

func (l *natsLogger) Noticef(format string, v ...any) { l.log.Info(fmt.Sprintf(format, v...)) }
func (l *natsLogger) Warnf(format string, v ...any)   { l.log.Warn(fmt.Sprintf(format, v...)) }
func (l *natsLogger) Errorf(format string, v ...any)  { l.log.Error(fmt.Sprintf(format, v...)) }
func (l *natsLogger) Fatalf(format string, v ...any)  { l.log.Error(fmt.Sprintf(format, v...)) }
func (l *natsLogger) Debugf(format string, v ...any)  { l.log.Debug(fmt.Sprintf(format, v...)) }
func (l *natsLogger) Tracef(format string, v ...any)  { l.log.Debug(fmt.Sprintf(format, v...)) }
