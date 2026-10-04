//go:build windows

package main

import (
	"encoding/json"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// configuredPanelURL resolves the panel URL from the installed config.  The
// installer must never guess a port: an upgrade keeps the operator's existing
// configs/client2api.json, so that file is the only trustworthy source.
func configuredPanelURL(dir string) (string, bool) {
	raw, err := os.ReadFile(filepath.Join(dir, "configs", "client2api.json"))
	if err != nil {
		return "", false
	}
	var cfg struct {
		Listen string `json:"listen"`
	}
	if err := json.Unmarshal([]byte(strings.TrimPrefix(string(raw), "\ufeff")), &cfg); err != nil {
		return "", false
	}
	return panelURLFromListen(cfg.Listen)
}

// panelURLFromListen turns a listen address into a browser URL.  Wildcard
// binds are replaced with loopback because 0.0.0.0 is not a connectable host.
func panelURLFromListen(listen string) (string, bool) {
	listen = strings.TrimSpace(listen)
	host, port, err := net.SplitHostPort(listen)
	if err != nil || strings.TrimSpace(port) == "" {
		return "", false
	}
	portNumber, err := strconv.Atoi(port)
	if err != nil || portNumber < 1 || portNumber > 65535 {
		return "", false
	}
	host = strings.TrimSpace(host)
	switch host {
	case "", "0.0.0.0", "::":
		host = "127.0.0.1"
	}
	if strings.HasPrefix(host, "[") || strings.HasSuffix(host, "]") {
		return "", false
	}
	if host == "*" || strings.ContainsAny(host, " \t\r\n/\\") {
		return "", false
	}
	return (&url.URL{Scheme: "http", Host: net.JoinHostPort(host, port), Path: "/panel/"}).String(), true
}
