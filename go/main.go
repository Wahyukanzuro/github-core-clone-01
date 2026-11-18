package main

import (
	"net"
	"os"
	"strings"
	"time"
)

// forge is a GitHub-compatible git forge: git smart HTTP, REST v3, GraphQL v4
// and MCP, served from an in-memory store by one process.
//
// Lifecycle (per the make.fun environment contract):
//   - MAKEFUN_ENV_URL must be set, else exit 1.
//   - Bind an ephemeral port (never 8080).
//   - POST {"port": N} to $MAKEFUN_ENV_URL/register until 200 or 409.
//   - Poll GET $MAKEFUN_ENV_URL/done until {"done": true}.
//   - Shut down and exit 0.

func main() {
	initTrialClock()
	envURL := strings.TrimSuffix(os.Getenv("MAKEFUN_ENV_URL"), "/")
	if envURL == "" {
		os.Exit(1)
	}
	token := os.Getenv("MAKEFUN_SHOP_TOKEN")

	store := NewStore(token)
	server := NewServer(store, token)

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		os.Exit(1)
	}
	port := ln.Addr().(*net.TCPAddr).Port

	go server.Serve(ln)

	// Register with the environment, retrying with backoff.
	registered := false
	delay := 100 * time.Millisecond
	deadline := time.Now().Add(120 * time.Second)
	for time.Now().Before(deadline) {
		status, _, err := httpDo("POST", envURL+"/register", jsonBytes(J{"port": port}), "application/json")
		if err == nil && (status == 200 || status == 409) {
			registered = true
			break
		}
		time.Sleep(delay)
		if delay < time.Second {
			delay *= 2
		}
	}
	if !registered {
		os.Exit(1)
	}

	// Poll /done until the environment says the trial is over.
	delay = 250 * time.Millisecond
	for {
		status, body, err := httpDo("GET", envURL+"/done", nil, "")
		if err == nil && status == 200 {
			m, ok := parseJSONBody(body)
			if ok {
				if v, present := m["done"]; present {
					if done, isBool := v.(bool); isBool && done {
						break
					}
				}
			}
		}
		time.Sleep(delay)
		if delay < 2*time.Second {
			delay *= 2
			if delay > 2*time.Second {
				delay = 2 * time.Second
			}
		}
	}

	ln.Close()
	os.Exit(0)
}
