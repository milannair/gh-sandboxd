package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"
)

func runRequest(t *testing.T, payload any) RunResponse {
	t.Helper()

	body, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "/run", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")

	rr := httptest.NewRecorder()
	runHandler(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("unexpected status: %d body=%s", rr.Code, rr.Body.String())
	}

	var resp RunResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal response: %v\nbody=%s", err, rr.Body.String())
	}

	return resp
}

func assertStdoutClean(t *testing.T, stdout string) {
	t.Helper()

	data, err := os.ReadFile(fcLog)
	if err != nil {
		t.Fatalf("read firecracker log: %v", err)
	}

	logText := strings.ReplaceAll(string(data), "\r\n", "\n")
	logText = strings.TrimSpace(logText)
	if logText == "" {
		t.Skip("firecracker log empty; cannot assert separation")
	}

	for _, line := range strings.Split(logText, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		if strings.Contains(stdout, line) {
			t.Fatalf("stdout contains firecracker log line: %q", line)
		}
	}
}

func TestSimpleEcho(t *testing.T) {
	resp := runRequest(t, map[string]any{
		"cmd":        "echo hi",
		"timeout_ms": 2000,
	})

	if resp.ExitCode != 0 {
		t.Fatalf("expected exit_code 0, got %d", resp.ExitCode)
	}
	if resp.Stderr != "" {
		t.Fatalf("expected empty stderr, got %q", resp.Stderr)
	}
	if !strings.Contains(resp.Stdout, "hi") {
		t.Fatalf("expected stdout to contain %q, got %q", "hi", resp.Stdout)
	}
	assertStdoutClean(t, resp.Stdout)
}

func TestBoundaryTimeout(t *testing.T) {
	resp := runRequest(t, map[string]any{
		"cmd":        "sleep 1",
		"timeout_ms": 1500,
	})

	if resp.ExitCode != 0 {
		t.Fatalf("expected exit_code 0, got %d", resp.ExitCode)
	}
	if resp.Stderr != "" {
		t.Fatalf("expected empty stderr, got %q", resp.Stderr)
	}
}

func TestHardTimeout(t *testing.T) {
	start := time.Now()

	resp := runRequest(t, map[string]any{
		"cmd":        "sleep 10",
		"timeout_ms": 1000,
	})

	if resp.ExitCode != 124 {
		t.Fatalf("expected exit_code 124, got %d", resp.ExitCode)
	}
	if resp.Stderr != "execution timed out" {
		t.Fatalf("expected timeout stderr, got %q", resp.Stderr)
	}

	if time.Since(start) > 3*time.Second {
		t.Fatalf("timeout test took too long")
	}
}

func TestFileInjection(t *testing.T) {
	resp := runRequest(t, map[string]any{
		"cmd": "sh main.sh",
		"files": map[string]string{
			"main.sh": "echo file ok",
		},
		"timeout_ms": 2000,
	})

	if resp.ExitCode != 0 {
		t.Fatalf("expected exit_code 0, got %d", resp.ExitCode)
	}
	if resp.Stderr != "" {
		t.Fatalf("expected empty stderr, got %q", resp.Stderr)
	}
	if !strings.Contains(resp.Stdout, "file ok") {
		t.Fatalf("expected stdout to contain %q, got %q", "file ok", resp.Stdout)
	}
}

func TestFileInjectionTimeout(t *testing.T) {
	resp := runRequest(t, map[string]any{
		"cmd": "sh main.sh",
		"files": map[string]string{
			"main.sh": "sleep 10",
		},
		"timeout_ms": 1000,
	})

	if resp.ExitCode != 124 {
		t.Fatalf("expected exit_code 124, got %d", resp.ExitCode)
	}
	if resp.Stderr != "execution timed out" {
		t.Fatalf("expected timeout stderr, got %q", resp.Stderr)
	}
}
