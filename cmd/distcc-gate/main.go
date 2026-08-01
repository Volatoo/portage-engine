// Command distcc-gate compares real local-only and distcc build evidence. It
// does not run or simulate distccd, PVE, installation, or GUI validation.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"time"

	"github.com/slchris/portage-engine/internal/distcc"
)

func main() {
	localPath := flag.String("local", "", "Local-only gate manifest")
	remotePath := flag.String("distcc", "", "Distcc gate manifest")
	outputPath := flag.String("output", "", "Optional atomic JSON receipt output")
	flag.Parse()
	if *localPath == "" || *remotePath == "" {
		log.Fatal("-local and -distcc evidence paths are required")
	}
	local, err := distcc.ReadGateManifest(*localPath)
	if err != nil {
		log.Fatalf("read local-only evidence: %v", err)
	}
	remote, err := distcc.ReadGateManifest(*remotePath)
	if err != nil {
		log.Fatalf("read distcc evidence: %v", err)
	}
	receipt, err := distcc.CompareGate(local, remote, time.Now())
	if err != nil {
		log.Fatalf("distcc comparison gate blocked: %v", err)
	}
	data, err := json.MarshalIndent(receipt, "", "  ")
	if err != nil {
		log.Fatal(err)
	}
	data = append(data, byte('\n'))
	if *outputPath != "" {
		if err := writeAtomic(*outputPath, data); err != nil {
			log.Fatalf("write gate receipt: %v", err)
		}
	}
	fmt.Print(string(data))
}

func writeAtomic(path string, data []byte) error {
	dir := filepath.Dir(path)
	file, err := os.CreateTemp(dir, ".distcc-gate-*")
	if err != nil {
		return err
	}
	temp := file.Name()
	defer func() { _ = os.Remove(temp) }()
	if err := file.Chmod(0o640); err != nil {
		_ = file.Close()
		return err
	}
	if _, err := file.Write(data); err != nil {
		_ = file.Close()
		return err
	}
	if err := file.Sync(); err != nil {
		_ = file.Close()
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	return os.Rename(temp, path)
}
