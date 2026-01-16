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
	"strings"
	"time"

	"github.com/google/uuid"
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
	fcLog      = "/tmp/firecracker/firecracker.log"
	kernelPath = "/home/milan/fc/hello-vmlinux.bin"
	rootfsPath = "/home/milan/fc/rootfs.ext4"
)

/* ---------------- Firecracker helpers ---------------- */

func startFirecracker() (*exec.Cmd, error) {
	_ = os.Remove(fcSocket)

	logDir := filepath.Dir(fcLog)
	if err := os.MkdirAll(logDir, 0o755); err != nil {
		return nil, err
	}
	logFile, err := os.Create(fcLog)
	if err != nil {
		return nil, err
	}
	_ = logFile.Close()

	cmd := exec.Command(
		"firecracker",
		"--api-sock", fcSocket,
		"--log-path", fcLog,
		"--level", "Error",
	)

	// Discard console output - we use UDS for results
	cmd.Stdout = nil
	cmd.Stderr = nil

	if err := cmd.Start(); err != nil {
		return nil, err
	}

	return cmd, nil
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

/* ---------------- UDS Agent Communication ---------------- */

// startUDSListener creates a per-run socket directory and listener.
func startUDSListener(execID string) (net.Listener, string, error) {
	baseDir := "/tmp/sandboxd"
	execDir := filepath.Join(baseDir, execID)

	if err := os.MkdirAll(execDir, 0o755); err != nil {
		return nil, "", err
	}

	sockPath := filepath.Join(execDir, "agent.sock")

	// Remove stale socket if any
	_ = os.Remove(sockPath)

	ln, err := net.Listen("unix", sockPath)
	if err != nil {
		return nil, "", err
	}

	return ln, sockPath, nil
}

// waitForAgentMessage waits for exactly one message from the guest agent.
func waitForAgentMessage(ln net.Listener, timeout time.Duration) (RunResponse, error) {
	type result struct {
		resp RunResponse
		err  error
	}

	ch := make(chan result, 1)

	go func() {
		conn, err := ln.Accept()
		if err != nil {
			ch <- result{err: err}
			return
		}
		defer conn.Close()

		dec := json.NewDecoder(conn)
		var resp RunResponse
		if err := dec.Decode(&resp); err != nil {
			ch <- result{err: err}
			return
		}

		ch <- result{resp: resp}
	}()

	select {
	case r := <-ch:
		return r.resp, r.err
	case <-time.After(timeout):
		return RunResponse{}, fmt.Errorf("timeout waiting for agent")
	}
}

// createAgentDriveImage creates a small ext4 image with the agent socket inside.
// Returns the image path and mount point.
func createAgentDriveImage(execID, sockPath string) (imagePath string, mountPoint string, cleanup func(), err error) {
	baseDir := "/tmp/sandboxd"
	execDir := filepath.Join(baseDir, execID)

	imagePath = filepath.Join(execDir, "agent.img")
	mountPoint = filepath.Join(execDir, "mnt")

	// Create a 1MB ext4 image
	if err := exec.Command("dd", "if=/dev/zero", "of="+imagePath, "bs=1M", "count=1").Run(); err != nil {
		return "", "", nil, fmt.Errorf("dd failed: %w", err)
	}

	if err := exec.Command("mkfs.ext4", "-F", imagePath).Run(); err != nil {
		return "", "", nil, fmt.Errorf("mkfs.ext4 failed: %w", err)
	}

	if err := os.MkdirAll(mountPoint, 0o755); err != nil {
		return "", "", nil, err
	}

	if err := exec.Command("mount", "-o", "loop", imagePath, mountPoint).Run(); err != nil {
		return "", "", nil, fmt.Errorf("mount failed: %w", err)
	}

	cleanup = func() {
		_ = exec.Command("umount", mountPoint).Run()
		_ = os.RemoveAll(execDir)
	}

	return imagePath, mountPoint, cleanup, nil
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

	// Generate unique execution ID
	execID := uuid.NewString()

	// Set up UDS listener for agent communication
	ln, sockPath, err := startUDSListener(execID)
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	defer ln.Close()
	defer os.RemoveAll(filepath.Dir(sockPath))

	// Create agent drive image and mount it
	agentImgPath, agentMountPoint, agentCleanup, err := createAgentDriveImage(execID, sockPath)
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	defer agentCleanup()

	// Create a symlink to the socket inside the agent mount point
	// The guest will mount this drive at /run/agent and find agent.sock there
	agentSockInMount := filepath.Join(agentMountPoint, "agent.sock")

	// We need to bind the socket into the mounted image's filesystem
	// Unix sockets can't be moved, so we create the original listener inside the mount
	ln.Close() // Close the original listener
	_ = os.Remove(sockPath)

	// Recreate the listener inside the mounted agent image
	ln, err = net.Listen("unix", agentSockInMount)
	if err != nil {
		http.Error(w, fmt.Sprintf("failed to create socket in agent mount: %v", err), 500)
		return
	}
	defer ln.Close()

	// Mount and prepare rootfs for files
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

	// Unmount the agent image before attaching to Firecracker
	if err := exec.Command("umount", agentMountPoint).Run(); err != nil {
		http.Error(w, fmt.Sprintf("failed to unmount agent image: %v", err), 500)
		return
	}

	fc, err := startFirecracker()
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}

	defer func() {
		if fc.Process != nil {
			_ = fc.Process.Kill()
		}
		_ = fc.Wait()
	}()

	if err := waitForSocket(fcSocket, 10*time.Second); err != nil {
		logText, readErr := os.ReadFile(fcLog)
		if readErr == nil {
			text := strings.ReplaceAll(string(logText), "\r\n", "\n")
			text = strings.TrimRight(text, "\n")
			lines := []string{}
			if text != "" {
				lines = strings.Split(text, "\n")
				if len(lines) > 50 {
					lines = lines[len(lines)-50:]
				}
			}
			snippet := strings.Join(lines, "\n")
			if snippet != "" {
				http.Error(w, fmt.Sprintf("%s\nfirecracker log:\n%s", err.Error(), snippet), 500)
				return
			}
		}
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

	// Mount the agent image as a secondary drive
	// The guest init will mount this at /run/agent
	if err := fcPut("/drives/agent", map[string]any{
		"drive_id":       "agent",
		"path_on_host":   agentImgPath,
		"is_root_device": false,
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

	// ---- Wait for agent response ----
	timeoutMs := req.TimeoutMs
	if timeoutMs <= 0 {
		timeoutMs = 5000
	}

	// Add boot grace period to timeout (5 seconds for kernel boot)
	totalTimeout := time.Duration(timeoutMs)*time.Millisecond + 5*time.Second

	resp, err := waitForAgentMessage(ln, totalTimeout)
	if err != nil {
		// Timeout or error
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(RunResponse{
			Stdout:   "",
			Stderr:   "execution timed out",
			ExitCode: 124,
		})
		return
	}

	// Success - return the agent's response
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(resp)
}

/* ---------------- main ---------------- */

func main() {
	http.HandleFunc("/run", runHandler)
	log.Println("sandboxd listening on :7777")
	log.Fatal(http.ListenAndServe(":7777", nil))
}
