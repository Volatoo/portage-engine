// Command desktop-runner executes deterministic GUI verification scenarios.
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"net"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/slchris/portage-engine/internal/desktop"
	"github.com/slchris/portage-engine/internal/imagefactory"
)

func main() {
	scenarioPath := flag.String("scenario", "", "Strict desktop scenario JSON")
	driverURL := flag.String("driver-url", "", "Desktop console adapter origin (mutually exclusive with -pve-config)")
	pveConfig := flag.String("pve-config", "", "Direct PVE/QGA driver policy JSON (mutually exclusive with -driver-url)")
	tokenEnv := flag.String("token-env", "PORTAGE_DESKTOP_DRIVER_TOKEN", "Environment variable containing the driver bearer token")
	pveTokenIDEnv := flag.String("pve-token-id-env", "PORTAGE_DESKTOP_PVE_TOKEN_ID", "Environment variable containing the PVE API token ID")
	pveTokenSecretEnv := flag.String("pve-token-secret-env", "PORTAGE_DESKTOP_PVE_TOKEN_SECRET", "Environment variable containing the PVE API token secret")
	output := flag.String("output", "", "Atomic result JSON output")
	allowHTTPControlPlane := flag.Bool("allow-http-control-plane", false, "Allow bearer-token HTTP to a trusted-LAN adapter (credentials are plaintext on the network)")
	allowHTTPLoopback := flag.Bool("allow-http-loopback", false, "Deprecated: allow HTTP only when the adapter host is loopback")
	flag.Parse()
	if *scenarioPath == "" || *output == "" || (*driverURL == "") == (*pveConfig == "") {
		log.Fatal("desktop-runner requires -scenario, -output, and exactly one of -driver-url or -pve-config")
	}
	scenario, err := desktop.LoadScenario(*scenarioPath)
	if err != nil {
		log.Fatal(err)
	}
	if err := scenario.ValidateRunnable(); err != nil {
		log.Fatal(err)
	}
	var driver desktop.Driver
	if *pveConfig != "" {
		config, err := desktop.LoadPVEConfig(*pveConfig)
		if err != nil {
			log.Fatal(err)
		}
		if err := config.ValidateScenario(scenario); err != nil {
			log.Fatal(err)
		}
		driver, err = desktop.NewPVEQGADriver(config, os.Getenv(*pveTokenIDEnv), os.Getenv(*pveTokenSecretEnv))
		if err != nil {
			log.Fatal(err)
		}
	} else {
		allowHTTP := *allowHTTPControlPlane
		if *allowHTTPLoopback {
			parsed, parseErr := url.Parse(strings.TrimSpace(*driverURL))
			host := ""
			if parseErr == nil {
				host = parsed.Hostname()
			}
			ip := net.ParseIP(host)
			if !strings.EqualFold(host, "localhost") && (ip == nil || !ip.IsLoopback()) {
				log.Fatal("-allow-http-loopback only permits a loopback adapter; use -allow-http-control-plane for a trusted LAN")
			}
			allowHTTP = true
		}
		driver, err = desktop.NewHTTPDriver(*driverURL, os.Getenv(*tokenEnv), allowHTTP)
		if err != nil {
			log.Fatal(err)
		}
	}
	result := desktop.Run(context.Background(), scenario, driver, time.Now)
	if err := imagefactory.WriteJSONAtomic(*output, result); err != nil {
		log.Fatal(err)
	}
	fmt.Printf("desktop scenario %s: %s\n", result.ScenarioID, result.State)
	if result.State != "passed" {
		os.Exit(1)
	}
}
