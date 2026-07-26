package server

import (
	"log"
	"net/http"
	"os/exec"
	"strings"

	"github.com/gorilla/websocket"
)

// handlers_shell.go bridges a WebSocket to an SSH session on a build instance
// so operators can inspect a node straight from the dashboard (web shell).

var shellUpgrader = websocket.Upgrader{
	ReadBufferSize:  4096,
	WriteBufferSize: 4096,
	// The dashboard proxies this endpoint; the server itself is API-key
	// protected, so cross-origin checks add nothing here.
	CheckOrigin: func(*http.Request) bool { return true },
}

// handleInstanceShell upgrades to a WebSocket and pipes it to an interactive
// SSH session on the requested instance (GET /api/v1/instances/shell?id=...).
func (s *Server) handleInstanceShell(w http.ResponseWriter, r *http.Request) {
	instanceID := r.URL.Query().Get("id")
	if instanceID == "" {
		http.Error(w, "id required", http.StatusBadRequest)
		return
	}

	var ip, sshUser string
	for _, inst := range s.builder.ListInstances() {
		if inst.ID == instanceID {
			ip = inst.IPAddress
			sshUser = inst.SSHUser
			break
		}
	}
	if ip == "" {
		http.Error(w, "instance not found", http.StatusNotFound)
		return
	}
	if sshUser == "" {
		sshUser = "root"
	}

	cs := s.builder.CloudSettings()
	if cs.SSHKeyPath == "" {
		http.Error(w, "no SSH key configured", http.StatusConflict)
		return
	}

	conn, err := shellUpgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	defer func() { _ = conn.Close() }()

	args := []string{"-tt", "-i", cs.SSHKeyPath, "-o", "ConnectTimeout=10"}
	if cs.SSHInsecureHostKey {
		args = append(args, "-o", "StrictHostKeyChecking=no", "-o", "UserKnownHostsFile=/dev/null")
	} else if cs.SSHKnownHosts != "" {
		args = append(args, "-o", "StrictHostKeyChecking=yes", "-o", "UserKnownHostsFile="+cs.SSHKnownHosts)
	}
	args = append(args, sshUser+"@"+ip, "stty cols 220 rows 50; exec bash -l")

	cmd := exec.Command("ssh", args...) // #nosec G204 -- operator-configured SSH parameters.
	stdin, err := cmd.StdinPipe()
	if err != nil {
		_ = conn.WriteMessage(websocket.TextMessage, []byte("failed to start ssh: "+err.Error()))
		return
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return
	}
	// StdoutPipe set cmd.Stdout to the pipe's write end; pointing stderr at it
	// merges both streams into what the client sees.
	cmd.Stderr = cmd.Stdout
	if err := cmd.Start(); err != nil {
		_ = conn.WriteMessage(websocket.TextMessage, []byte("failed to start ssh: "+err.Error()))
		return
	}
	defer func() { _ = cmd.Process.Kill() }()

	// WS -> ssh stdin
	go func() {
		for {
			_, data, err := conn.ReadMessage()
			if err != nil {
				_ = stdin.Close()
				return
			}
			if _, err := stdin.Write(data); err != nil {
				return
			}
		}
	}()

	// ssh stdout -> WS
	buf := make([]byte, 4096)
	for {
		n, err := stdout.Read(buf)
		if n > 0 {
			if wErr := conn.WriteMessage(websocket.BinaryMessage, buf[:n]); wErr != nil {
				break
			}
		}
		if err != nil {
			break
		}
	}
	_ = cmd.Wait()
	log.Printf("shell session to %q (%q) closed", instanceID, strings.TrimSpace(ip)) // #nosec G706 -- values are safely quoted.
}
