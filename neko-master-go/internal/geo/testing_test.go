package geo

import (
	"log/slog"
	"os"
	"testing"
)

// testLogger 返回一个写到测试输出的 logger。
func testLogger(t *testing.T) *slog.Logger {
	t.Helper()
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
}
