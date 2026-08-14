package iac

import (
	"context"
	"crypto/tls"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// pve_scheduler.go implements automatic node placement for PVE build VMs
// (CLOUD_PVE_NODE=auto). Proxmox itself has no scheduler for API-created VMs —
// a clone must name an explicit target node (the built-in CRS only manages
// HA-registered resources) — so placement is decided here: the cluster's live
// load is read via GET /api2/json/cluster/resources and the least-loaded
// eligible node wins.

// pveNodeQueryTimeout bounds the cluster-resources API call so a hung PVE API
// cannot stall a build worker.
const pveNodeQueryTimeout = 10 * time.Second

var pveCleanupPollInterval = 2 * time.Second

// pveClusterResource is one entry of GET /api2/json/cluster/resources. The
// same list carries nodes (type "node") and guests (type "qemu"/"lxc").
type pveClusterResource struct {
	Type     string  `json:"type"`
	Node     string  `json:"node"`
	Name     string  `json:"name"`
	Status   string  `json:"status"`
	VMID     int     `json:"vmid"`
	Template int     `json:"template"`
	MaxMem   int64   `json:"maxmem"`
	Mem      int64   `json:"mem"`
	CPU      float64 `json:"cpu"`
}

// PVEAuth carries the credentials for direct PVE API calls. Either an API
// token (preferred) or username+password (exchanged for a ticket) must be set.
type PVEAuth struct {
	TokenID     string
	TokenSecret string
	Username    string
	Password    string
	Insecure    bool
}

func (a PVEAuth) valid() bool {
	return (a.TokenID != "" && a.TokenSecret != "") || (a.Username != "" && a.Password != "")
}

// SelectPVENode picks the node a new build VM should be cloned onto: the
// online node with the most free memory (ties broken by lower CPU load).
//
// Eligibility: when candidates is non-empty it is the allowed placement list —
// the operator asserts the template is usable from every listed node (shared
// storage, or a same-named template copy per node). When empty, placement is
// restricted to nodes that host a template named templateName, which is
// clone-safe regardless of storage type.
func SelectPVENode(endpoint string, auth PVEAuth, candidates []string, templateName string) (string, error) {
	if !auth.valid() {
		return "", fmt.Errorf("automatic node selection requires PVE credentials (API token or username/password)")
	}

	resources, err := fetchPVEClusterResources(endpoint, auth)
	if err != nil {
		return "", err
	}

	// Determine the eligible node set.
	eligible := make(map[string]bool)
	if len(candidates) > 0 {
		for _, c := range candidates {
			if c = strings.TrimSpace(c); c != "" {
				eligible[c] = true
			}
		}
	} else {
		for _, r := range resources {
			if (r.Type == "qemu") && r.Template == 1 && r.Name == templateName {
				eligible[r.Node] = true
			}
		}
		if len(eligible) == 0 {
			return "", fmt.Errorf("no node in the cluster hosts a template named %q (set CLOUD_PVE_NODES to name placement candidates explicitly)", templateName)
		}
	}

	// Pick the least-loaded online eligible node.
	var best *pveClusterResource
	for i := range resources {
		r := &resources[i]
		if r.Type != "node" || r.Status != "online" || !eligible[r.Node] {
			continue
		}
		if best == nil || nodeLessLoaded(r, best) {
			best = r
		}
	}
	if best == nil {
		return "", fmt.Errorf("no eligible PVE node is online (eligible: %s)", strings.Join(mapKeys(eligible), ", "))
	}
	return best.Node, nil
}

// PVENodeInfo summarizes one cluster node for connectivity tests and UI
// display.
type PVENodeInfo struct {
	Node        string  `json:"node"`
	Status      string  `json:"status"`
	FreeMemGB   float64 `json:"free_mem_gb"`
	CPULoad     float64 `json:"cpu_load"`
	HasTemplate bool    `json:"has_template"`
}

// PVEClusterNodes lists the cluster's nodes with load info. templateName, when
// non-empty, marks which nodes host a template of that name. Used by the
// dashboard's "test connection" flow to validate endpoint + credentials +
// template in one call.
func PVEClusterNodes(endpoint string, auth PVEAuth, templateName string) ([]PVENodeInfo, error) {
	if !auth.valid() {
		return nil, fmt.Errorf("PVE credentials are required (API token or username/password)")
	}
	resources, err := fetchPVEClusterResources(endpoint, auth)
	if err != nil {
		return nil, err
	}

	templateNodes := make(map[string]bool)
	for _, r := range resources {
		if r.Type == "qemu" && r.Template == 1 && r.Name == templateName {
			templateNodes[r.Node] = true
		}
	}

	nodes := make([]PVENodeInfo, 0, len(resources))
	for _, r := range resources {
		if r.Type != "node" {
			continue
		}
		nodes = append(nodes, PVENodeInfo{
			Node:        r.Node,
			Status:      r.Status,
			FreeMemGB:   float64(r.MaxMem-r.Mem) / (1 << 30),
			CPULoad:     r.CPU,
			HasTemplate: templateNodes[r.Node],
		})
	}
	return nodes, nil
}

// nodeLessLoaded reports whether a is a better placement target than b:
// more free memory, ties broken by lower CPU load.
func nodeLessLoaded(a, b *pveClusterResource) bool {
	freeA, freeB := a.MaxMem-a.Mem, b.MaxMem-b.Mem
	if freeA != freeB {
		return freeA > freeB
	}
	return a.CPU < b.CPU
}

// pveHTTPClient returns the shared client used for direct PVE API calls. Both
// clients are built once: WaitForPVEGuestIP polls every 5s for up to 5 minutes
// and PVEGuestExec every 500ms, and a per-request Transport leaks its idle
// connections and their reader goroutines for the life of the process.
var (
	pveSecureClient   = &http.Client{Timeout: pveNodeQueryTimeout}
	pveInsecureClient = &http.Client{
		Timeout:   pveNodeQueryTimeout,
		Transport: pveInsecureTransport(),
	}
)

func pveInsecureTransport() *http.Transport {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.TLSClientConfig = &tls.Config{InsecureSkipVerify: true} // #nosec G402 -- mirrors CLOUD_PVE_INSECURE, the operator's explicit opt-in for self-signed PVE certs.
	return transport
}

func pveHTTPClient(insecure bool) *http.Client {
	if insecure {
		return pveInsecureClient
	}
	return pveSecureClient
}

// validatePVEEndpoint constrains the base URL before any credential is put on
// the wire. Every caller here appends its own "/api2/json/..." suffix and
// attaches either the API token header or — on the ticket path — the account
// password as a cleartext form body, so the endpoint alone decides where that
// credential is delivered. It must therefore name a host and nothing else:
// userinfo would smuggle a second set of credentials into the request, and a
// path, query or fragment would swallow the appended suffix, so
// "https://pve.internal:8006/?x=" quietly turns the cluster-resources call into
// a request for "/" with the API path riding in the query string. Plain HTTP is
// accepted only together with the operator's explicit insecure opt-in, because
// a trusted-LAN Proxmox such as http://10.31.0.200:8006 is a real deployment
// while an unannounced downgrade to cleartext is not.
func validatePVEEndpoint(endpoint string, insecure bool) error {
	trimmed := strings.TrimRight(endpoint, "/")
	if strings.TrimSpace(trimmed) == "" {
		return fmt.Errorf("PVE endpoint is required")
	}
	parsed, err := url.Parse(trimmed)
	if err != nil {
		return fmt.Errorf("PVE endpoint %q is not a valid URL: %w", endpoint, err)
	}
	switch {
	case parsed.Scheme == "https":
	case parsed.Scheme == "http" && insecure:
	case parsed.Scheme == "http":
		return fmt.Errorf("PVE endpoint %q uses plain HTTP; enable the insecure option to allow it on a trusted LAN", endpoint)
	default:
		return fmt.Errorf("PVE endpoint %q must use https (or http with the insecure option)", endpoint)
	}
	if parsed.Hostname() == "" {
		return fmt.Errorf("PVE endpoint %q names no host", endpoint)
	}
	// The reconstruction is the real invariant: it also rejects the bare "?" and
	// "#" that url.Parse records as an empty query or fragment while the string
	// still captures everything appended after it.
	if parsed.User != nil || parsed.Path != "" || parsed.RawQuery != "" || parsed.Fragment != "" ||
		trimmed != parsed.Scheme+"://"+parsed.Host {
		return fmt.Errorf("PVE endpoint %q must be a bare scheme://host[:port] without userinfo, path, query, or fragment", endpoint)
	}
	return nil
}

// pveEndpointShape is the lexical form validatePVEEndpoint accepts: a bare
// scheme://host[:port] with nothing else. Every function that concatenates a
// request URL re-checks the exact string it concatenates, because a barrier
// must sit on that value: validatePVEEndpoint's semantic checks live in
// another function and do not mark the caller's variable as clean.
var pveEndpointShape = regexp.MustCompile(`^https?://[A-Za-z0-9._\-\[\]:]+$`)

// pveTicket exchanges username+password for an auth ticket (the PVE cookie
// auth flow — API tokens don't need this).
func pveTicket(endpoint string, auth PVEAuth) (string, error) {
	ticket, _, err := pveLogin(context.Background(), endpoint, auth)
	return ticket, err
}

func pveLogin(ctx context.Context, endpoint string, auth PVEAuth) (string, string, error) {
	if err := validatePVEEndpoint(endpoint, auth.Insecure); err != nil {
		return "", "", err
	}
	base := strings.TrimRight(endpoint, "/")
	if !pveEndpointShape.MatchString(base) {
		return "", "", fmt.Errorf("PVE endpoint %q must be a bare scheme://host[:port]", endpoint)
	}
	form := url.Values{}
	form.Set("username", auth.Username)
	form.Set("password", auth.Password)

	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		base+"/api2/json/access/ticket",
		strings.NewReader(form.Encode()))
	if err != nil {
		return "", "", fmt.Errorf("build ticket request: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := pveHTTPClient(auth.Insecure).Do(req)
	if err != nil {
		return "", "", fmt.Errorf("PVE ticket request: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return "", "", fmt.Errorf("PVE authentication failed (status %d) — check username/password", resp.StatusCode)
	}

	var payload struct {
		Data struct {
			Ticket              string `json:"ticket"`
			CSRFPreventionToken string `json:"CSRFPreventionToken"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		return "", "", fmt.Errorf("decode ticket response: %w", err)
	}
	if payload.Data.Ticket == "" {
		return "", "", fmt.Errorf("PVE returned an empty auth ticket")
	}
	return payload.Data.Ticket, payload.Data.CSRFPreventionToken, nil
}

type pveAPIClient struct {
	endpoint string
	auth     PVEAuth
	ticket   string
	csrf     string
	client   *http.Client
}

func newPVEAPIClient(ctx context.Context, endpoint string, auth PVEAuth) (*pveAPIClient, error) {
	if err := validatePVEEndpoint(endpoint, auth.Insecure); err != nil {
		return nil, err
	}
	if !auth.valid() {
		return nil, fmt.Errorf("PVE credentials are required")
	}
	base := strings.TrimRight(endpoint, "/")
	if !pveEndpointShape.MatchString(base) {
		return nil, fmt.Errorf("PVE endpoint %q must be a bare scheme://host[:port]", endpoint)
	}
	client := &pveAPIClient{
		endpoint: base,
		auth:     auth,
		client:   pveHTTPClient(auth.Insecure),
	}
	if auth.TokenID == "" || auth.TokenSecret == "" {
		ticket, csrf, err := pveLogin(ctx, endpoint, auth)
		if err != nil {
			return nil, err
		}
		client.ticket = ticket
		client.csrf = csrf
	}
	return client, nil
}

func (c *pveAPIClient) do(ctx context.Context, method, path string, form url.Values, output any) error {
	var body io.Reader
	requestURL := c.endpoint + path
	// PVE's API proxy rejects request bodies on DELETE with HTTP 501. DELETE
	// parameters must be encoded in the query string.
	if method == http.MethodDelete && len(form) > 0 {
		separator := "?"
		if strings.Contains(requestURL, "?") {
			separator = "&"
		}
		requestURL += separator + form.Encode()
	} else if len(form) > 0 {
		body = strings.NewReader(form.Encode())
	}
	req, err := http.NewRequestWithContext(ctx, method, requestURL, body)
	if err != nil {
		return err
	}
	if len(form) > 0 && method != http.MethodDelete {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	if c.ticket != "" {
		req.AddCookie(pveAuthCookie(c.ticket))
		if method != http.MethodGet && method != http.MethodHead {
			req.Header.Set("CSRFPreventionToken", c.csrf)
		}
	} else {
		req.Header.Set("Authorization", fmt.Sprintf("PVEAPIToken=%s=%s", c.auth.TokenID, c.auth.TokenSecret))
	}
	resp, err := c.client.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		detail, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("PVE API %s %s returned status %d: %s",
			method, path, resp.StatusCode, strings.TrimSpace(string(detail)))
	}
	if output != nil {
		if err := json.NewDecoder(resp.Body).Decode(output); err != nil {
			return err
		}
	}
	return nil
}

func (c *pveAPIClient) clusterResources(ctx context.Context) ([]pveClusterResource, error) {
	var payload struct {
		Data []pveClusterResource `json:"data"`
	}
	err := c.do(ctx, http.MethodGet, "/api2/json/cluster/resources?type=vm", nil, &payload)
	return payload.Data, err
}

// EnsurePVEVMAbsent closes the state-before-resource crash window in the
// Terraform provider. A killed apply can leave an asynchronous clone in PVE
// before the provider writes it to terraform.tfstate; `terraform destroy`
// then reports success while doing nothing. This function discovers only the
// exact pre-registered builder name, waits out any clone lock, and removes it
// through the PVE API. A clone inherits the template's tags until Terraform
// reaches its later configuration step, so tags cannot be an ownership
// requirement in precisely this pre-state crash window. The exact name comes
// from the private workspace manifest written before apply and cannot be
// supplied through the public build request.
func EnsurePVEVMAbsent(ctx context.Context, endpoint string, auth PVEAuth, expectedNode, expectedName string) error {
	if !strings.HasPrefix(expectedName, "portage-builder-") &&
		!strings.HasPrefix(expectedName, "portage-capacity-") {
		return fmt.Errorf("refusing PVE cleanup for unexpected resource name %q", expectedName)
	}
	client, err := newPVEAPIClient(ctx, endpoint, auth)
	if err != nil {
		return err
	}

	for {
		resources, err := client.clusterResources(ctx)
		if err != nil {
			return fmt.Errorf("discover PVE cleanup target: %w", err)
		}
		var matches []pveClusterResource
		for _, resource := range resources {
			if resource.Type == "qemu" && resource.Template == 0 && resource.Name == expectedName {
				matches = append(matches, resource)
			}
		}
		if len(matches) == 0 {
			return nil
		}
		if len(matches) != 1 {
			return fmt.Errorf("PVE cleanup identity %q matched %d VMs", expectedName, len(matches))
		}
		target := matches[0]
		if target.VMID <= 0 || target.Node == "" {
			return fmt.Errorf("PVE cleanup target %q has incomplete identity", expectedName)
		}
		if expectedNode != "" && target.Node != expectedNode {
			// Migration is legitimate, but make the mismatch explicit while
			// still using the authoritative current node returned by PVE.
			expectedNode = target.Node
		}

		configPath := fmt.Sprintf("/api2/json/nodes/%s/qemu/%d/config",
			url.PathEscape(target.Node), target.VMID)
		var configPayload struct {
			Data struct {
				Name string `json:"name"`
				Tags string `json:"tags"`
				Lock string `json:"lock"`
			} `json:"data"`
		}
		if err := client.do(ctx, http.MethodGet, configPath, nil, &configPayload); err != nil {
			return fmt.Errorf("inspect PVE cleanup target: %w", err)
		}
		if configPayload.Data.Name != expectedName {
			return fmt.Errorf("refusing to delete mismatched PVE VM %d (%q, expected %q)",
				target.VMID, configPayload.Data.Name, expectedName)
		}
		if configPayload.Data.Lock != "" {
			if err := waitPVEInterval(ctx, pveCleanupPollInterval); err != nil {
				return fmt.Errorf("wait for PVE VM %d lock %q: %w", target.VMID, configPayload.Data.Lock, err)
			}
			continue
		}
		if target.Status != "stopped" {
			stopPath := fmt.Sprintf("/api2/json/nodes/%s/qemu/%d/status/stop",
				url.PathEscape(target.Node), target.VMID)
			if err := client.do(ctx, http.MethodPost, stopPath, url.Values{}, nil); err != nil {
				return fmt.Errorf("stop residual PVE VM %d: %w", target.VMID, err)
			}
			if err := waitPVEInterval(ctx, pveCleanupPollInterval); err != nil {
				return err
			}
			continue
		}

		deletePath := fmt.Sprintf("/api2/json/nodes/%s/qemu/%d",
			url.PathEscape(target.Node), target.VMID)
		deleteForm := url.Values{
			"purge":                      {"1"},
			"destroy-unreferenced-disks": {"1"},
		}
		if err := client.do(ctx, http.MethodDelete, deletePath, deleteForm, nil); err != nil {
			return fmt.Errorf("delete residual PVE VM %d: %w", target.VMID, err)
		}
		if err := waitPVEInterval(ctx, pveCleanupPollInterval); err != nil {
			return err
		}
	}
}

func waitPVEInterval(ctx context.Context, interval time.Duration) error {
	timer := time.NewTimer(interval)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

// fetchPVEClusterResources reads the cluster resource list, authenticating
// with an API token when available, else a username/password ticket.
func fetchPVEClusterResources(endpoint string, auth PVEAuth) ([]pveClusterResource, error) {
	if err := validatePVEEndpoint(endpoint, auth.Insecure); err != nil {
		return nil, err
	}
	base := strings.TrimRight(endpoint, "/")
	if !pveEndpointShape.MatchString(base) {
		return nil, fmt.Errorf("PVE endpoint %q must be a bare scheme://host[:port]", endpoint)
	}
	reqURL := base + "/api2/json/cluster/resources"
	req, err := http.NewRequest(http.MethodGet, reqURL, nil)
	if err != nil {
		return nil, fmt.Errorf("build cluster-resources request: %w", err)
	}

	if auth.TokenID != "" && auth.TokenSecret != "" {
		req.Header.Set("Authorization", fmt.Sprintf("PVEAPIToken=%s=%s", auth.TokenID, auth.TokenSecret))
	} else {
		ticket, err := pveTicket(endpoint, auth)
		if err != nil {
			return nil, err
		}
		req.AddCookie(pveAuthCookie(ticket))
	}

	resp, err := pveHTTPClient(auth.Insecure).Do(req)
	if err != nil {
		return nil, fmt.Errorf("query PVE cluster resources: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("PVE cluster resources returned status %d", resp.StatusCode)
	}

	var payload struct {
		Data []pveClusterResource `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		return nil, fmt.Errorf("decode cluster resources: %w", err)
	}
	return payload.Data, nil
}

// WaitForPVEGuestIP polls the QEMU guest agent through the PVE API until the
// VM reports a usable IPv4 address. telmate rc04's default_ipv4_address is
// frequently empty right after apply (the agent hasn't reported yet), so the
// IP is resolved here instead of failing the deployment on an empty address.
func WaitForPVEGuestIP(endpoint string, auth PVEAuth, node string, vmid string, timeout time.Duration, sink func(string)) (string, error) {
	if err := validatePVEEndpoint(endpoint, auth.Insecure); err != nil {
		return "", err
	}
	deadline := time.Now().Add(timeout)
	reqURL := fmt.Sprintf("%s/api2/json/nodes/%s/qemu/%s/agent/network-get-interfaces",
		strings.TrimRight(endpoint, "/"), node, vmid)

	var ticket string
	if auth.TokenID == "" || auth.TokenSecret == "" {
		t, err := pveTicket(endpoint, auth)
		if err != nil {
			return "", err
		}
		ticket = t
	}

	attempt := 0
	for time.Now().Before(deadline) {
		attempt++
		guestAgentReady := false
		req, err := http.NewRequest(http.MethodGet, reqURL, nil)
		if err != nil {
			return "", err
		}
		if ticket != "" {
			req.AddCookie(pveAuthCookie(ticket))
		} else {
			req.Header.Set("Authorization", fmt.Sprintf("PVEAPIToken=%s=%s", auth.TokenID, auth.TokenSecret))
		}

		resp, err := pveHTTPClient(auth.Insecure).Do(req)
		if err == nil && resp.StatusCode == http.StatusOK {
			var payload struct {
				Data struct {
					Result []struct {
						Name string `json:"name"`
						IPs  []struct {
							Type    string `json:"ip-address-type"`
							Address string `json:"ip-address"`
						} `json:"ip-addresses"`
					} `json:"result"`
				} `json:"data"`
			}
			decodeErr := json.NewDecoder(resp.Body).Decode(&payload)
			_ = resp.Body.Close()
			if decodeErr == nil {
				guestAgentReady = true
				for _, iface := range payload.Data.Result {
					if iface.Name == "lo" || strings.HasPrefix(iface.Name, "docker") {
						continue
					}
					for _, ip := range iface.IPs {
						if ip.Type == "ipv4" && ip.Address != "" && !strings.HasPrefix(ip.Address, "127.") {
							return ip.Address, nil
						}
					}
				}
			}
		} else if resp != nil {
			_ = resp.Body.Close()
		}

		// PVE can boot a successor with the source template's rendered
		// networkd file, then replace it only when cloud-init's delayed network
		// stage runs. QGA is already reachable in that state, but the new NIC
		// never acquires DHCP until networkd reloads the current renderer output.
		// Retry a fixed, no-input recovery while there is still no IPv4 address;
		// newly built templates also carry the equivalent boot-time unit.
		if guestAgentReady && attempt%6 == 0 {
			recoveryDeadline := time.Now().Add(10 * time.Second)
			if deadline.Before(recoveryDeadline) {
				recoveryDeadline = deadline
			}
			recoveryCtx, cancel := context.WithDeadline(context.Background(), recoveryDeadline)
			recoveryErr := recoverPVEGuestNetwork(recoveryCtx, endpoint, auth, node, vmid)
			cancel()
			if recoveryErr == nil && sink != nil {
				sink("[provision] guest agent is ready without IPv4; reloaded the clone's cloud-init network configuration")
			}
		}
		if attempt%6 == 0 && sink != nil {
			sink(fmt.Sprintf("[provision] still waiting for the guest agent to report an IP (%ds)…", attempt*5))
		}
		time.Sleep(5 * time.Second)
	}
	return "", fmt.Errorf("guest agent did not report an IPv4 address within %s (is qemu-guest-agent installed in the template?)", timeout)
}

const pveGuestNetworkRecoveryScript = `set -eu
network_file=/etc/systemd/network/10-cloud-init-eth0.network
dropin_dir=${network_file}.d
if [ -f "$network_file" ]; then
  install -d -m 0755 "$dropin_dir"
  printf '[DHCPv4]\nClientIdentifier=mac\n' >"$dropin_dir/90-portage-engine-dhcp.conf"
  chmod 0644 "$dropin_dir/90-portage-engine-dhcp.conf"
fi
systemctl --no-block restart systemd-networkd.service`

func recoverPVEGuestNetwork(
	ctx context.Context,
	endpoint string,
	auth PVEAuth,
	node string,
	vmid string,
) error {
	_, err := PVEGuestExec(ctx, endpoint, auth, node, vmid, []string{
		"/bin/sh", "-c", pveGuestNetworkRecoveryScript,
	})
	return err
}

// PVEGuestExec runs one bounded command through the authenticated PVE/QEMU
// guest-agent channel. Callers must not retry stateful commands after an
// ambiguous response. It is used for read-only image identity material and
// for the fixed, idempotent networkd recovery above; arbitrary user commands
// must never be passed to it.
func PVEGuestExec(ctx context.Context, endpoint string, auth PVEAuth, node, vmid string, command []string) (string, error) {
	if !auth.valid() {
		return "", fmt.Errorf("PVE guest exec requires credentials")
	}
	if node == "" || vmid == "" || len(command) == 0 {
		return "", fmt.Errorf("PVE guest exec requires node, VMID, and command")
	}
	if err := validatePVEEndpoint(endpoint, auth.Insecure); err != nil {
		return "", err
	}
	values := url.Values{}
	for _, argument := range command {
		if argument == "" || len(argument) > 4096 || strings.ContainsRune(argument, '\x00') {
			return "", fmt.Errorf("invalid PVE guest command argument")
		}
		values.Add("command", argument)
	}

	ticket, csrf := "", ""
	if auth.TokenID == "" || auth.TokenSecret == "" {
		var err error
		ticket, csrf, err = pveLogin(ctx, endpoint, auth)
		if err != nil {
			return "", err
		}
	}
	authorize := func(request *http.Request) {
		if ticket != "" {
			request.AddCookie(pveAuthCookie(ticket))
			if request.Method != http.MethodGet && request.Method != http.MethodHead {
				request.Header.Set("CSRFPreventionToken", csrf)
			}
		} else {
			request.Header.Set("Authorization", fmt.Sprintf("PVEAPIToken=%s=%s", auth.TokenID, auth.TokenSecret))
		}
	}

	base := fmt.Sprintf("%s/api2/json/nodes/%s/qemu/%s/agent",
		strings.TrimRight(endpoint, "/"), url.PathEscape(node), url.PathEscape(vmid))
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, base+"/exec", strings.NewReader(values.Encode()))
	if err != nil {
		return "", err
	}
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	authorize(request)
	response, err := pveHTTPClient(auth.Insecure).Do(request)
	if err != nil {
		return "", fmt.Errorf("start PVE guest exec: %w", err)
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		message, _ := io.ReadAll(io.LimitReader(response.Body, 4096))
		return "", fmt.Errorf("PVE guest exec returned HTTP %d: %s", response.StatusCode, strings.TrimSpace(string(message)))
	}
	var started struct {
		Data struct {
			PID int `json:"pid"`
		} `json:"data"`
	}
	if err := json.NewDecoder(io.LimitReader(response.Body, 1<<20)).Decode(&started); err != nil || started.Data.PID < 1 {
		return "", fmt.Errorf("PVE guest exec returned an invalid PID")
	}

	for {
		query := url.Values{"pid": []string{strconv.Itoa(started.Data.PID)}}
		statusRequest, err := http.NewRequestWithContext(ctx, http.MethodGet, base+"/exec-status?"+query.Encode(), nil)
		if err != nil {
			return "", err
		}
		authorize(statusRequest)
		statusResponse, err := pveHTTPClient(auth.Insecure).Do(statusRequest)
		if err != nil {
			return "", fmt.Errorf("query PVE guest exec: %w", err)
		}
		if statusResponse.StatusCode < 200 || statusResponse.StatusCode >= 300 {
			message, _ := io.ReadAll(io.LimitReader(statusResponse.Body, 4096))
			_ = statusResponse.Body.Close()
			return "", fmt.Errorf("PVE guest exec status returned HTTP %d: %s", statusResponse.StatusCode, strings.TrimSpace(string(message)))
		}
		var status struct {
			Data struct {
				Exited       int    `json:"exited"`
				ExitCode     int    `json:"exitcode"`
				OutData      string `json:"out-data"`
				ErrData      string `json:"err-data"`
				OutTruncated int    `json:"out-truncated"`
				ErrTruncated int    `json:"err-truncated"`
			} `json:"data"`
		}
		decodeErr := json.NewDecoder(io.LimitReader(statusResponse.Body, 1<<20)).Decode(&status)
		_ = statusResponse.Body.Close()
		if decodeErr != nil {
			return "", fmt.Errorf("decode PVE guest exec status: %w", decodeErr)
		}
		if status.Data.Exited == 1 {
			if status.Data.OutTruncated != 0 || status.Data.ErrTruncated != 0 {
				return "", fmt.Errorf("PVE guest exec output was truncated")
			}
			if status.Data.ExitCode != 0 {
				message := strings.TrimSpace(status.Data.ErrData)
				if message == "" {
					message = strings.TrimSpace(status.Data.OutData)
				}
				return "", fmt.Errorf("PVE guest exec exited %d: %s", status.Data.ExitCode, message)
			}
			return status.Data.OutData, nil
		}
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-time.After(500 * time.Millisecond):
		}
	}
}

// WaitForPVEGuestHostKey obtains the guest's ED25519 SSH public host key over
// QGA, so a smoke runner can pin it before the first SSH connection.
func WaitForPVEGuestHostKey(endpoint string, auth PVEAuth, node, vmid string, timeout time.Duration) (string, error) {
	deadline := time.Now().Add(timeout)
	var lastErr error
	for time.Now().Before(deadline) {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		output, err := PVEGuestExec(ctx, endpoint, auth, node, vmid,
			[]string{"/usr/bin/cat", "/etc/ssh/ssh_host_ed25519_key.pub"})
		cancel()
		if err == nil {
			fields := strings.Fields(output)
			if len(fields) >= 2 && len(fields) <= 3 && fields[0] == "ssh-ed25519" {
				decoded, decodeErr := base64.StdEncoding.DecodeString(fields[1])
				if decodeErr == nil && len(decoded) == 51 && binary.BigEndian.Uint32(decoded[:4]) == 11 && string(decoded[4:15]) == "ssh-ed25519" && binary.BigEndian.Uint32(decoded[15:19]) == 32 {
					return fields[0] + " " + fields[1], nil
				}
			}
			lastErr = fmt.Errorf("guest returned an invalid ED25519 SSH host key")
		} else {
			lastErr = err
		}
		time.Sleep(2 * time.Second)
	}
	return "", fmt.Errorf("guest ED25519 SSH host key was not available within %s: %w", timeout, lastErr)
}

func pveAuthCookie(ticket string) *http.Cookie {
	return &http.Cookie{
		Name:     "PVEAuthCookie",
		Value:    ticket,
		Secure:   true,
		HttpOnly: true,
		SameSite: http.SameSiteStrictMode,
	}
}

// mapKeys returns the keys of a string set for error messages.
func mapKeys(m map[string]bool) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	return keys
}
