package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"github.com/tatarinovs/booster/internal/version"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
)

func main() {
	arch := flag.String("arch", "amd64", "Windows architecture: amd64 or 386")
	flag.Parse()
	if err := generate(*arch); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func generate(arch string) error {
	manifestArch := "amd64"
	if arch == "386" {
		manifestArch = "x86"
	} else if arch != "amd64" {
		return fmt.Errorf("unsupported architecture: %s", arch)
	}
	parts := strings.Split(version.Number, ".")
	if len(parts) != 3 {
		return fmt.Errorf("invalid VERSION: %q", version.Number)
	}
	numbers := make([]int, 3)
	for i, part := range parts {
		n, err := strconv.Atoi(part)
		if err != nil || n < 0 || n > 65535 || strconv.Itoa(n) != part {
			return fmt.Errorf("invalid VERSION: %q", version.Number)
		}
		numbers[i] = n
	}
	data, err := os.ReadFile("versioninfo.template.json")
	if err != nil {
		return err
	}
	var info map[string]interface{}
	if err = json.Unmarshal(data, &info); err != nil {
		return err
	}
	fixed := info["FixedFileInfo"].(map[string]interface{})
	fields := info["StringFileInfo"].(map[string]interface{})
	for _, key := range []string{"FileVersion", "ProductVersion"} {
		fixed[key] = map[string]int{"Major": numbers[0], "Minor": numbers[1], "Patch": numbers[2], "Build": 0}
		fields[key] = version.Number + ".0"
	}
	data, err = json.Marshal(info)
	if err != nil {
		return err
	}
	temp, err := os.MkdirTemp("", "booster-resources-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(temp)
	config := filepath.Join(temp, "versioninfo.json")
	if err = os.WriteFile(config, data, 0600); err != nil {
		return err
	}
	manifest, err := os.ReadFile("booster.exe.manifest.tmpl")
	if err != nil {
		return err
	}
	manifest = []byte(strings.NewReplacer("{{VERSION}}", version.Number+".0", "{{ARCH}}", manifestArch).Replace(string(manifest)))
	manifestPath := filepath.Join(temp, "booster.exe.manifest")
	if err = os.WriteFile(manifestPath, manifest, 0600); err != nil {
		return err
	}
	cmd := exec.Command("goversioninfo", "-icon=favicon.ico", "-manifest="+manifestPath, "-64="+strconv.FormatBool(arch == "amd64"), config)
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	return cmd.Run()
}
