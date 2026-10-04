package main

import (
	"bytes"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"testing"
)

// These tests guard the packaging files the way the panel's shell tests guard
// index.html: they pin the things that only fail in a place nobody is looking --
// a Docker image that exposes the wrong port, an example config that has drifted
// from the struct, a release workflow grepping for a declaration that no longer
// exists.  All of them are pure text assertions against files in this repo, so
// they run offline like everything else.

// repoFile reads a file relative to the repository root.  Tests run with the
// package directory as their working directory, so every path walks up twice.
func repoFile(t *testing.T, rel string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "..", filepath.FromSlash(rel)))
	if err != nil {
		t.Fatalf("read %s: %v", rel, err)
	}
	return string(b)
}

// TestDockerImageMatchesTheProgramsOwnDefaults ties the image to the program:
// the exposed port is derived from applyDefaults rather than written down twice,
// so changing the default listen address cannot silently leave the image
// publishing a port nothing listens on.
func TestDockerImageMatchesTheProgramsOwnDefaults(t *testing.T) {
	cfg := &fileConfig{}
	cfg.applyDefaults()
	_, port, err := net.SplitHostPort(cfg.Listen)
	if err != nil {
		t.Fatalf("applyDefaults produced an unparseable listen address %q: %v", cfg.Listen, err)
	}

	dockerfile := repoFile(t, "Dockerfile")
	for _, want := range []string{
		"EXPOSE " + port,
		"/healthz",         // the probe must hit readiness, not just a socket
		"./cmd/client2api", // the one binary this project builds
		"USER app",         // never run the gateway as root
		"CGO_ENABLED=0",    // static binary; the runtime image has no toolchain
		// ENTRYPOINT is a JSON array, so the flags arrive as separate quoted
		// tokens rather than as one "flag value" string.
		`"-config", "/app/configs/client2api.json"`,
		`"-listen", "0.0.0.0:` + port + `"`, // loopback is unreachable from outside a container
	} {
		if !strings.Contains(dockerfile, want) {
			t.Errorf("Dockerfile is missing %q", want)
		}
	}

	compose := repoFile(t, "docker-compose.yml")
	for _, want := range []string{
		port + ":" + port,
		"./configs:/app/configs",
		"./data:/app/data",
	} {
		if !strings.Contains(compose, want) {
			t.Errorf("docker-compose.yml is missing %q", want)
		}
	}
}

// TestPackagingIgnoresSecretsAndBinaries checks the two ignore files.  data/
// holds live access tokens and the repository root holds 18 MB executables, so
// a missing rule here is not cosmetic.  logs/ is asserted too: it is covered by
// neither data/ nor *.log, and it holds real chat payloads the probes wrote.
//
// bin/ is the Makefile's output directory.  *.exe covers its contents on
// Windows, but the default target has no extension there, so the rule is named
// explicitly -- otherwise `make build` on a Linux dev box drops two untracked
// 13 MB binaries into the repository root forever.
func TestPackagingIgnoresSecretsAndBinaries(t *testing.T) {
	for _, tc := range []struct {
		file string
		want []string
	}{
		{"dockerignore", []string{"*.exe", "configs/client2api.json", ".git/", "logs/", "bin/"}},
		{"gitignore", []string{"data/", "configs/client2api.json", "*.exe", "logs/", "bin/"}},
	} {
		body := repoFile(t, "."+tc.file)
		for _, want := range tc.want {
			if !strings.Contains(body, want) {
				t.Errorf(".%s does not ignore %q", tc.file, want)
			}
		}
	}
}

// TestExampleConfigMatchesTheStruct decodes the shipped example into fileConfig
// with unknown fields rejected, then checks the other direction too: every JSON
// tag the struct declares must actually appear in the file.  A stale example is
// worse than no example, because the operator trusts it.
func TestExampleConfigMatchesTheStruct(t *testing.T) {
	body := repoFile(t, "configs/client2api.example.json")

	dec := json.NewDecoder(bytes.NewReader([]byte(body)))
	dec.DisallowUnknownFields()
	var cfg fileConfig
	if err := dec.Decode(&cfg); err != nil {
		t.Fatalf("configs/client2api.example.json does not decode into fileConfig: %v", err)
	}

	var present map[string]any
	if err := json.Unmarshal([]byte(body), &present); err != nil {
		t.Fatalf("example config is not a JSON object: %v", err)
	}
	typ := reflect.TypeOf(fileConfig{})
	for i := 0; i < typ.NumField(); i++ {
		tag := strings.Split(typ.Field(i).Tag.Get("json"), ",")[0]
		if tag == "" || tag == "-" {
			continue
		}
		if _, ok := present[tag]; !ok {
			t.Errorf("fileConfig declares %q but the example config never mentions it", tag)
		}
	}
}

// TestReleaseWorkflowReadsTheVersionFromThisFile pins both halves of the version
// plumbing: the workflow's grep must point at the file that declares `version`,
// and the declaration must still be the shape that grep expects.  The pattern
// itself uses \K, which Go's regexp cannot run, so the declaration is checked
// with an equivalent pattern instead of by executing the workflow's one.
func TestReleaseWorkflowReadsTheVersionFromThisFile(t *testing.T) {
	wf := repoFile(t, ".github/workflows/go-binaries.yml")
	if !strings.Contains(wf, `var version = "\K[^"]+`) {
		t.Fatalf("go-binaries.yml no longer greps for the version declaration")
	}
	if !strings.Contains(wf, "cmd/client2api/main.go") {
		t.Fatalf("go-binaries.yml reads the version from somewhere other than main.go")
	}
	if !strings.Contains(wf, "-X main.version=") {
		t.Fatalf("go-binaries.yml never injects the version, so the tag and the binary can disagree")
	}

	src := repoFile(t, "cmd/client2api/main.go")
	m := regexp.MustCompile(`var version = "([^"]+)"`).FindStringSubmatch(src)
	if m == nil {
		t.Fatalf("cmd/client2api/main.go no longer declares `var version = \"...\"`")
	}
	if m[1] != version {
		t.Errorf("main.go declares version %q but the built-in value is %q", m[1], version)
	}

	// The archives must carry something runnable: an operator who unpacks one
	// and finds no config has to guess every key name.
	if !strings.Contains(wf, "configs/client2api.example.json") {
		t.Errorf("go-binaries.yml does not bundle the example config")
	}
	if _, err := os.Stat(filepath.Join("..", "..", "configs", "client2api.example.json")); err != nil {
		t.Errorf("the workflow bundles a file that does not exist: %v", err)
	}
}

// TestMakefileReadsTheVersionFromThisFile pins the Makefile's version plumbing
// the same way the workflow's is pinned above.  `VERSION ?= $(shell sed -n 's/^var
// version = "\(.*\)"$/\1/p' ...)` fails open: rename the declaration and sed prints
// nothing, VERSION becomes the empty string, and `make build` still succeeds --
// it just stamps `-X main.version=` into the binary, which then disagrees with
// the tag it was released under.  Nothing about that is loud, so the sed program
// has to keep matching the same shape the workflow greps for.
func TestMakefileReadsTheVersionFromThisFile(t *testing.T) {
	mk := repoFile(t, "Makefile")
	if !strings.Contains(mk, "-X main.version=$(VERSION)") {
		t.Fatalf("Makefile never injects $(VERSION) into the binary")
	}
	if !strings.Contains(mk, `sed -n 's/^var version = "\(.*\)"$$/\1/p' cmd/client2api/main.go`) {
		t.Fatalf("Makefile no longer extracts the version the way the release workflow reads it")
	}

	// That sed program only matches one source shape, so check the shape itself.
	src := repoFile(t, "cmd/client2api/main.go")
	m := regexp.MustCompile(`(?m)^var version = "([^"]*)"$`).FindStringSubmatch(src)
	if m == nil {
		t.Fatalf("cmd/client2api/main.go has no line of the form `var version = \"...\"` for the Makefile's sed to match")
	}
	if m[1] != version {
		t.Errorf("sed would extract %q but the built-in version is %q", m[1], version)
	}
}
