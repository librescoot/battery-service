package battery

import (
	"log"
)

type LogLevel int

const (
	LogLevelNone LogLevel = iota
	LogLevelError
	LogLevelWarning
	LogLevelInfo
	LogLevelDebug
)

type Logger struct {
	logger *log.Logger
	level  LogLevel
}

func NewLogger(logger *log.Logger, level LogLevel) *Logger {
	return &Logger{
		logger: logger,
		level:  level,
	}
}

func (l *Logger) Debugf(format string, v ...interface{}) {
	if l.level >= LogLevelDebug {
		l.logger.Printf("DEBUG: "+format, v...)
	}
}

func (l *Logger) Infof(format string, v ...interface{}) {
	if l.level >= LogLevelInfo {
		l.logger.Printf(format, v...)
	}
}

func (l *Logger) Warnf(format string, v ...interface{}) {
	if l.level >= LogLevelWarning {
		l.logger.Printf("WARN: "+format, v...)
	}
}

func (l *Logger) Errorf(format string, v ...interface{}) {
	if l.level >= LogLevelError {
		l.logger.Printf("ERROR: "+format, v...)
	}
}

func (l *Logger) Fatalf(format string, v ...interface{}) {
	l.logger.Fatalf("FATAL: "+format, v...)
}
