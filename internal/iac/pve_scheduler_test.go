package iac

import (
	"context"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestEnsurePVEVMAbsentDeletesTaggedResidualAfterLockClears(t *testing.T) {
	oldInterval := pveCleanupPollInterval
	pveCleanupPollInterval = time.Millisecond
	t.Cleanup(func() { pveCleanupPollInterval = oldInterval })

	var configReads atomic.Int32
	var deleted atomic.Bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "PVEAPIToken=cleanup@pve!test=secret" {
			t.Fatalf("Authorization = %q", got)
		}
		switch r.URL.Path {
		case "/api2/json/cluster/resources":
			if deleted.Load() {
				_ = json.NewEncoder(w).Encode(map[string]any{"data": []any{}})
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"data": []any{map[string]any{
				"type": "qemu", "node": "pve01", "name": "portage-builder-amd64-123",
				"status": "stopped", "vmid": 901, "template": 0,
			}}})
		case "/api2/json/nodes/pve01/qemu/901/config":
			lock := ""
			if configReads.Add(1) == 1 {
				lock = "clone"
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{
				"name": "portage-builder-amd64-123", "tags": "portage-builder;ephemeral", "lock": lock,
			}})
		case "/api2/json/nodes/pve01/qemu/901":
			if r.Method != http.MethodDelete {
				t.Fatalf("method = %s", r.Method)
			}
			if r.URL.Query().Get("purge") != "1" || r.URL.Query().Get("destroy-unreferenced-disks") != "1" {
				t.Fatalf("delete query = %v", r.URL.Query())
			}
			deleted.Store(true)
			_ = json.NewEncoder(w).Encode(map[string]any{"data": "UPID:test"})
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	err := EnsurePVEVMAbsent(ctx, server.URL,
		PVEAuth{TokenID: "cleanup@pve!test", TokenSecret: "secret"},
		"pve01", "portage-builder-amd64-123")
	if err != nil {
		t.Fatal(err)
	}
	if !deleted.Load() {
		t.Fatal("residual VM was not deleted")
	}
}

func TestEnsurePVEVMAbsentRefusesUnexpectedResourceName(t *testing.T) {
	var deleteCalled atomic.Bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		deleteCalled.Store(true)
		http.NotFound(w, r)
	}))
	defer server.Close()

	err := EnsurePVEVMAbsent(context.Background(), server.URL,
		PVEAuth{TokenID: "cleanup@pve!test", TokenSecret: "secret"},
		"pve01", "production-database")
	if err == nil || !strings.Contains(err.Error(), "refusing PVE cleanup") {
		t.Fatalf("error = %v", err)
	}
	if deleteCalled.Load() {
		t.Fatal("delete was attempted for an untagged VM")
	}
}

func TestEnsurePVEVMAbsentUsesPasswordCSRFForDelete(t *testing.T) {
	oldInterval := pveCleanupPollInterval
	pveCleanupPollInterval = time.Millisecond
	t.Cleanup(func() { pveCleanupPollInterval = oldInterval })

	var deleted atomic.Bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api2/json/access/ticket":
			_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{
				"ticket": "PVE:ticket", "CSRFPreventionToken": "csrf-token",
			}})
		case "/api2/json/cluster/resources":
			if deleted.Load() {
				_ = json.NewEncoder(w).Encode(map[string]any{"data": []any{}})
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"data": []any{map[string]any{
				"type": "qemu", "node": "pve01", "name": "portage-builder-amd64-456",
				"status": "stopped", "vmid": 902, "template": 0,
			}}})
		case "/api2/json/nodes/pve01/qemu/902/config":
			_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{
				"name": "portage-builder-amd64-456", "tags": "candidate;template-tags",
			}})
		case "/api2/json/nodes/pve01/qemu/902":
			cookie, err := r.Cookie("PVEAuthCookie")
			if err != nil || cookie.Value != "PVE:ticket" {
				t.Fatalf("auth cookie = %v, err=%v", cookie, err)
			}
			if got := r.Header.Get("CSRFPreventionToken"); got != "csrf-token" {
				t.Fatalf("CSRF token = %q", got)
			}
			deleted.Store(true)
			_ = json.NewEncoder(w).Encode(map[string]any{"data": "UPID:test"})
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	err := EnsurePVEVMAbsent(context.Background(), server.URL,
		PVEAuth{Username: "cleanup@pve", Password: "password"},
		"pve01", "portage-builder-amd64-456")
	if err != nil {
		t.Fatal(err)
	}
	if !deleted.Load() {
		t.Fatal("password-authenticated cleanup did not delete the VM")
	}
}

func TestWaitForPVEGuestHostKeyBindsKeyThroughQGA(t *testing.T) {
	raw := make([]byte, 51)
	binary.BigEndian.PutUint32(raw[:4], 11)
	copy(raw[4:15], "ssh-ed25519")
	binary.BigEndian.PutUint32(raw[15:19], 32)
	for index := 19; index < len(raw); index++ {
		raw[index] = byte(index)
	}
	encoded := base64.StdEncoding.EncodeToString(raw)

	var mutex sync.Mutex
	var command []string
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.Header.Get("Authorization") != "PVEAPIToken=smoke@pve!gate=secret" {
			t.Errorf("unexpected authorization header")
		}
		writer.Header().Set("Content-Type", "application/json")
		switch request.URL.Path {
		case "/api2/json/nodes/pve01/qemu/900/agent/exec":
			if err := request.ParseForm(); err != nil {
				t.Error(err)
			}
			mutex.Lock()
			command = append([]string(nil), request.PostForm["command"]...)
			mutex.Unlock()
			_, _ = fmt.Fprint(writer, `{"data":{"pid":42}}`)
		case "/api2/json/nodes/pve01/qemu/900/agent/exec-status":
			if request.URL.Query().Get("pid") != "42" {
				t.Errorf("unexpected PID query")
			}
			_, _ = fmt.Fprintf(writer, `{"data":{"exited":1,"exitcode":0,"out-data":"ssh-ed25519 %s guest\n","err-data":"","out-truncated":0,"err-truncated":0}}`, encoded)
		default:
			http.NotFound(writer, request)
		}
	}))
	defer server.Close()

	key, err := WaitForPVEGuestHostKey(server.URL, PVEAuth{TokenID: "smoke@pve!gate", TokenSecret: "secret"}, "pve01", "900", 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if key != "ssh-ed25519 "+encoded {
		t.Fatalf("unexpected key %q", key)
	}
	mutex.Lock()
	defer mutex.Unlock()
	if !slices.Equal(command, []string{"/usr/bin/cat", "/etc/ssh/ssh_host_ed25519_key.pub"}) {
		t.Fatalf("unexpected QGA command %#v", command)
	}
}

// fakeCluster serves a /api2/json/cluster/resources fixture and records the
// Authorization header it saw.
func fakeCluster(t *testing.T) (*httptest.Server, *string) {
	t.Helper()
	var gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api2/json/cluster/resources" {
			http.NotFound(w, r)
			return
		}
		gotAuth = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(w, clusterFixture)
	}))
	t.Cleanup(srv.Close)
	return srv, &gotAuth
}

const clusterFixture = `{"data":[
	{"type":"node","node":"pve1","status":"online","maxmem":68719476736,"mem":60129542144,"cpu":0.10},
	{"type":"node","node":"pve2","status":"online","maxmem":68719476736,"mem":17179869184,"cpu":0.50},
	{"type":"node","node":"pve3","status":"offline","maxmem":137438953472,"mem":0,"cpu":0},
	{"type":"qemu","node":"pve1","name":"debian-12-cloudinit-template","template":1,"status":"stopped"},
	{"type":"qemu","node":"pve2","name":"debian-12-cloudinit-template","template":1,"status":"stopped"},
	{"type":"qemu","node":"pve2","name":"some-running-vm","template":0,"status":"running"}
]}`

func TestSelectPVENode_PicksLeastLoadedTemplateNode(t *testing.T) {
	srv, gotAuth := fakeCluster(t)

	// No candidate list: restrict to nodes hosting the template (pve1, pve2).
	// pve2 has far more free memory and must win; offline pve3 is ignored.
	node, err := SelectPVENode(srv.URL, PVEAuth{TokenID: "root@pam!terraform", TokenSecret: "s3cr3t"}, nil, "debian-12-cloudinit-template")
	if err != nil {
		t.Fatalf("SelectPVENode: %v", err)
	}
	if node != "pve2" {
		t.Errorf("expected pve2 (most free memory), got %q", node)
	}
	if want := "PVEAPIToken=root@pam!terraform=s3cr3t"; *gotAuth != want {
		t.Errorf("auth header = %q, want %q", *gotAuth, want)
	}
}

func TestSelectPVENode_CandidateListWins(t *testing.T) {
	srv, _ := fakeCluster(t)

	// Explicit candidates override the template restriction: only pve1 allowed.
	node, err := SelectPVENode(srv.URL, PVEAuth{TokenID: "root@pam!t", TokenSecret: "s"}, []string{"pve1"}, "debian-12-cloudinit-template")
	if err != nil {
		t.Fatalf("SelectPVENode: %v", err)
	}
	if node != "pve1" {
		t.Errorf("expected pve1 (only candidate), got %q", node)
	}
}

func TestSelectPVENode_TemplateMissing(t *testing.T) {
	srv, _ := fakeCluster(t)

	if _, err := SelectPVENode(srv.URL, PVEAuth{TokenID: "root@pam!t", TokenSecret: "s"}, nil, "no-such-template"); err == nil {
		t.Fatal("expected an error when no node hosts the template")
	}
}

func TestSelectPVENode_AllEligibleOffline(t *testing.T) {
	srv, _ := fakeCluster(t)

	if _, err := SelectPVENode(srv.URL, PVEAuth{TokenID: "root@pam!t", TokenSecret: "s"}, []string{"pve3"}, ""); err == nil {
		t.Fatal("expected an error when every eligible node is offline")
	}
}

func TestSelectPVENode_RequiresTokenAuth(t *testing.T) {
	if _, err := SelectPVENode("https://pve:8006", PVEAuth{}, nil, "tpl"); err == nil {
		t.Fatal("expected an error without token credentials")
	}
}

func TestNodeLessLoaded(t *testing.T) {
	moreFree := &pveClusterResource{MaxMem: 100, Mem: 10, CPU: 0.9}
	lessFree := &pveClusterResource{MaxMem: 100, Mem: 50, CPU: 0.1}
	if !nodeLessLoaded(moreFree, lessFree) {
		t.Error("node with more free memory should win regardless of CPU")
	}

	tiedA := &pveClusterResource{MaxMem: 100, Mem: 50, CPU: 0.2}
	tiedB := &pveClusterResource{MaxMem: 100, Mem: 50, CPU: 0.8}
	if !nodeLessLoaded(tiedA, tiedB) {
		t.Error("on equal free memory, lower CPU load should win")
	}
}
