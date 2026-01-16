package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

type RunRequest struct {
	Cmd       string            `json:"cmd"`
	Files     map[string]string `json:"files"`
	TimeoutMs int               `json:"timeout_ms"`
}

type RunResponse struct {
	Stdout   string `json:"stdout"`
	Stderr   string `json:"stderr"`
	ExitCode int    `json:"exit_code"`
}

const (
	fcSocket   = "/tmp/fc.sock"
	fcConsole  = "/tmp/guest-console.log"
	fcLog      = "/tmp/firecracker/firecracker.log"
	kernelPath = "/home/milan/fc/hello-vmlinux.bin"
	rootfsPath = "/home/milan/fc/rootfs.ext4"
)

/* ---------------- Firecracker helpers ---------------- */

func startFirecracker() (*exec.Cmd, *os.File, error) {
	_ = os.Remove(fcSocket)
	_ = os.Remove(fcConsole)

	logDir := filepath.Dir(fcLog)
	if err := os.MkdirAll(logDir, 0o755); err != nil {
		return nil, nil, err
	}

	consoleFile, err := os.Create(fcConsole)
	if err != nil {
		return nil, nil, err
	}

	cmd := exec.Command(
		"firecracker",
		"--api-sock", fcSocket,
		"--log-path", fcLog,
		"--level", "Error",
	)

	cmd.Stdout = consoleFile
	cmd.Stderr = nil

	if err := cmd.Start(); err != nil {
		_ = consoleFile.Close()
		return nil, nil, err
	}

	return cmd, consoleFile, nil
}

func waitForSocket(path string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(path); err == nil {
			return nil
		}
		time.Sleep(25 * time.Millisecond)
	}
	return fmt.Errorf("timeout waiting for socket %s", path)
}

func fcPut(path string, body any) error {
	data, err := json.Marshal(body)
	if err != nil {
		return err
	}

	tr := &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return net.Dial("unix", fcSocket)
		},
	}

	client := &http.Client{
		Transport: tr,
		Timeout:   5 * time.Second,
	}

	req, err := http.NewRequest("PUT", "http://unix"+path, bytes.NewReader(data))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusNoContent {
		b, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("firecracker %s failed: %s", path, strings.TrimSpace(string(b)))
	}

	return nil
}

/* ---------------- Guest console parsing ---------------- */

// Wait until the guest init actually starts (so we don't count boot time against timeout_ms).
func waitForGuestInitStarted(timeout time.Duration) error {
	deadline := time.Now().Add(timeout)

	for time.Now().Before(deadline) {
		b, err := os.ReadFile(fcConsole)
		if err == nil {
			text := strings.ReplaceAll(string(b), "\r\n", "\n")
			if strings.Contains(text, "[guest] init started") {
				return nil
			}
			// If the guest already halted/panicked, don't wait forever.
			if strings.Contains(text, "Kernel panic") || strings.Contains(text, "reboot: System halted") {
				return fmt.Errorf("guest did not reach init started (halt/panic)")
			}
		}
		time.Sleep(50 * time.Millisecond)
	}

	return fmt.Errorf("timeout waiting for guest init started")
}

func waitForGuestCompletion(timeout time.Duration) (stdout string, exitCode int, err error) {
	deadline := time.Now().Add(timeout)

	for time.Now().Before(deadline) {
		b, readErr := os.ReadFile(fcConsole)
		if readErr == nil {
			text := strings.ReplaceAll(string(b), "\r\n", "\n")

			for _, line := range strings.Split(text, "\n") {
				if strings.HasPrefix(line, "[guest] exit code:") {
					parts := strings.Split(line, ":")
					if len(parts) == 2 {
						code, _ := strconv.Atoi(strings.TrimSpace(parts[1]))
						return text, code, nil
					}
				}
			}

			if strings.Contains(text, "reboot: System halted") {
				return text, 0, nil
			}
		}

		time.Sleep(50 * time.Millisecond)
	}

	b, _ := os.ReadFile(fcConsole)
	text := strings.ReplaceAll(string(b), "\r\n", "\n")
	return text, 124, fmt.Errorf("timeout waiting for guest completion")
}

func resolveWorkPath(workDir, name string) (string, error) {
	if name == "" {
		return "", fmt.Errorf("file name is empty")
	}
	if filepath.IsAbs(name) {
		return "", fmt.Errorf("absolute paths are not allowed")
	}
	clean := filepath.Clean(name)
	if clean == "." || clean == ".." || strings.HasPrefix(clean, ".."+string(os.PathSeparator)) {
		return "", fmt.Errorf("path traversal is not allowed")
	}
	targetPath := filepath.Join(workDir, clean)
	rel, err := filepath.Rel(workDir, targetPath)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(os.PathSeparator)) {
		return "", fmt.Errorf("path escapes work dir")
	}
	return targetPath, nil
}

/* ---------------- HTTP handler ---------------- */

func runHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}

	var req RunRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid JSON", http.StatusBadRequest)
		return
	}
	if req.Cmd == "" {
		http.Error(w, "cmd is required", http.StatusBadRequest)
		return
	}

	log.Printf("run: %q", req.Cmd)

	mountDir, err := os.MkdirTemp("", "rootfs-mount-")
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	defer os.RemoveAll(mountDir)

	mountCmd := exec.Command("mount", "-o", "loop", rootfsPath, mountDir)
	if err := mountCmd.Run(); err != nil {
		http.Error(w, err.Error(), 500)
		return
	}

	unmountErr := func() error {
		return exec.Command("umount", mountDir).Run()
	}

	workDir := mountDir + "/work"
	if err := os.MkdirAll(workDir, 0o755); err != nil {
		_ = unmountErr()
		http.Error(w, err.Error(), 500)
		return
	}

	for name, content := range req.Files {
		targetPath, err := resolveWorkPath(workDir, name)
		if err != nil {
			_ = unmountErr()
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		if err := os.WriteFile(targetPath, []byte(content), 0o644); err != nil {
			_ = unmountErr()
			http.Error(w, err.Error(), 500)
			return
		}
		if strings.HasPrefix(content, "#!") {
			if err := os.Chmod(targetPath, 0o755); err != nil {
				_ = unmountErr()
				http.Error(w, err.Error(), 500)
				return
			}
		}
	}

	if err := unmountErr(); err != nil {
		http.Error(w, err.Error(), 500)
		return
	}

	fc, consoleFile, err := startFirecracker()
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	defer consoleFile.Close()

	defer func() {
		if fc.Process != nil {
			_ = fc.Process.Kill()
		}
		_ = fc.Wait()
	}()

	if err := waitForSocket(fcSocket, 10*time.Second); err != nil {
		http.Error(w, err.Error(), 500)
		return
	}

	if err := fcPut("/machine-config", map[string]any{
		"vcpu_count":   1,
		"mem_size_mib": 256,
		"smt":          false,
	}); err != nil {
		http.Error(w, err.Error(), 500)
		return
	}

	cmdForGuest := req.Cmd
	if len(req.Files) > 0 {
		cmdForGuest = fmt.Sprintf("cd /work && %s", req.Cmd)
	}
	bootArgs := fmt.Sprintf(
		"console=ttyS0 quiet loglevel=0 reboot=k panic=1 pci=off init=/sbin/init CMD=\"%s\"",
		cmdForGuest,
	)

	if err := fcPut("/boot-source", map[string]any{
		"kernel_image_path": kernelPath,
		"boot_args":         bootArgs,
	}); err != nil {
		http.Error(w, err.Error(), 500)
		return
	}

	if err := fcPut("/drives/rootfs", map[string]any{
		"drive_id":       "rootfs",
		"path_on_host":   rootfsPath,
		"is_root_device": true,
		"is_read_only":   false,
	}); err != nil {
		http.Error(w, err.Error(), 500)
		return
	}

	if err := fcPut("/actions", map[string]any{
		"action_type": "InstanceStart",
	}); err != nil {
		http.Error(w, err.Error(), 500)
		return
	}

	// ---- Timeout semantics fix ----
	timeoutMs := req.TimeoutMs
	if timeoutMs <= 0 {
		timeoutMs = 5000
	}

	// Boot grace: wait for init-start marker (does not consume timeout_ms).
	// If boot is slow, fail with a clear error.
	if err := waitForGuestInitStarted(5 * time.Second); err != nil {
		resp := RunResponse{
			Stdout:   "",
			Stderr:   "boot timeout: " + err.Error(),
			ExitCode: 124,
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
		return
	}

	// Now start the real execution timeout.
	done := make(chan struct{})
	var (
		stdout   string
		exitCode int
		waitErr  error
	)

	go func() {
		stdout, exitCode, waitErr = waitForGuestCompletion(time.Duration(timeoutMs) * time.Millisecond)
		close(done)
	}()

	timer := time.NewTimer(time.Duration(timeoutMs) * time.Millisecond)
	defer timer.Stop()

	select {
	case <-done:
		timer.Stop()
		if fc.Process != nil {
			_ = fc.Process.Kill()
		}
		_ = fc.Wait()

		stderr := ""
		if waitErr != nil {
			stderr = waitErr.Error()
		}

		resp := RunResponse{
			Stdout:   stdout,
			Stderr:   stderr,
			ExitCode: exitCode,
		}

		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
		return

	case <-timer.C:
		if fc.Process != nil {
			_ = fc.Process.Kill()
		}
		_ = fc.Wait()

		resp := RunResponse{
			Stdout:   "",
			Stderr:   "execution timed out",
			ExitCode: 124,
		}

		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
		return
	}
}

/* ---------------- main ---------------- */

func main() {
	http.HandleFunc("/run", runHandler)
	log.Println("sandboxd listening on :7777")
	log.Fatal(http.ListenAndServe(":7777", nil))
}
