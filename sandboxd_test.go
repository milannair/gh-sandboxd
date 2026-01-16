package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
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

func TestSimpleEcho(t *testing.T) {
	resp := runRequest(t, map[string]any{
		"cmd":        "echo hi",
		"timeout_ms": 2000,
	})

	if resp.ExitCode != 0 {
		t.Fatalf("expected exit_code 0, got %d", resp.ExitCode)
	}
	if resp.Stdout == "" {
		t.Fatalf("expected stdout, got empty")
	}
}

func TestBoundaryTimeout(t *testing.T) {
	resp := runRequest(t, map[string]any{
		"cmd":        "sleep 1",
		"timeout_ms": 1500,
	})

	if resp.ExitCode != 0 {
		t.Fatalf("expected exit_code 0, got %d", resp.ExitCode)
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
	if resp.Stdout == "" {
		t.Fatalf("expected stdout, got empty")
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
}
