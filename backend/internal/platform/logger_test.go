package platform

import (
	"io"
	"os"
	"strings"
	"testing"

	"newtg/gateway/internal/config"
)

// captureStdout 临时接管 os.Stdout，返回读取已捕获输出的函数。
func captureStdout(t *testing.T) func() string {
	t.Helper()
	old := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe: %v", err)
	}
	os.Stdout = w
	return func() string {
		os.Stdout = old
		w.Close()
		b, err := io.ReadAll(r)
		if err != nil {
			t.Fatalf("read captured stdout: %v", err)
		}
		r.Close()
		return string(b)
	}
}

// TestNewLoggerJSON：LOG_FORMAT=json 时 handler 输出为 JSON 且含 "level":"INFO" 键。
func TestNewLoggerJSON(t *testing.T) {
	cfg := &config.Config{
		AppEnv:    "production",
		LogLevel:  "info",
		LogFormat: "json",
	}
	read := captureStdout(t)

	log := NewLogger(cfg)
	log.Info("hello newtg")

	out := read()
	if !strings.Contains(out, `"level":"INFO"`) {
		t.Fatalf("json log missing level key, got: %q", out)
	}
}
