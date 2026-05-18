package serviceutil

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

const (
	httpReadHeaderTimeout   = 10 * time.Second
	httpShutdownTimeout     = 5 * time.Second
	clientLeasePollInterval = 500 * time.Millisecond
	clientLeaseEmptyGrace   = 10 * time.Minute
)

func ListenInRange(host, portRange string) (net.Listener, error) {
	parts := strings.SplitN(portRange, "-", 2)
	if len(parts) != 2 {
		return nil, fmt.Errorf("invalid port range %q: expected \"lo-hi\"", portRange)
	}
	lo, err1 := strconv.Atoi(strings.TrimSpace(parts[0]))
	hi, err2 := strconv.Atoi(strings.TrimSpace(parts[1]))
	if err1 != nil || err2 != nil || lo < 1 || hi > 65535 || lo > hi {
		return nil, fmt.Errorf("invalid port range %q", portRange)
	}
	for port := lo; port <= hi; port++ {
		addr := fmt.Sprintf("%s:%d", host, port)
		ln, err := net.Listen("tcp", addr)
		if err == nil {
			return ln, nil
		}
	}
	return nil, fmt.Errorf("no available port in range %s", portRange)
}

func StartControlServer(ctx context.Context, shutdownHTTP func(context.Context) error, network string, socketPath string, listenAddr string, serviceAddr string) (func(), error) {
	if network == "unix" {
		if err := os.MkdirAll(filepath.Dir(socketPath), 0o700); err != nil {
			return nil, err
		}
		if err := os.Remove(socketPath); err != nil && !errors.Is(err, os.ErrNotExist) {
			return nil, err
		}
	}

	ln, err := net.Listen(network, firstNonEmpty(listenAddr, socketPath))
	if err != nil {
		return nil, err
	}
	if network == "unix" {
		if chmodErr := os.Chmod(socketPath, 0o600); chmodErr != nil {
			ln.Close()
			_ = os.Remove(socketPath)
			return nil, chmodErr
		}
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /service-addr", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{
			"serviceAddr": serviceAddr,
			"serviceURL":  "http://" + serviceAddr,
		})
	})
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{
			"ok":          true,
			"serviceAddr": serviceAddr,
		})
	})
	mux.HandleFunc("POST /shutdown", func(w http.ResponseWriter, _ *http.Request) {
		log.Printf("[INFO] shutdown requested via control socket")
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})
		if shutdownHTTP == nil {
			return
		}
		go func() {
			shutdownCtx, cancel := context.WithTimeout(context.Background(), httpShutdownTimeout)
			defer cancel()
			if err := shutdownHTTP(shutdownCtx); err != nil && !errors.Is(err, http.ErrServerClosed) {
				log.Printf("[WARN] shutdown via control socket: %v", err)
			}
		}()
	})

	controlServer := &http.Server{
		Handler:           mux,
		ReadHeaderTimeout: httpReadHeaderTimeout,
	}

	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), httpShutdownTimeout)
		defer cancel()
		_ = controlServer.Shutdown(shutdownCtx)
	}()

	go func() {
		if serveErr := controlServer.Serve(ln); serveErr != nil && !errors.Is(serveErr, http.ErrServerClosed) {
			log.Printf("[WARN] control socket server: %v", serveErr)
		}
	}()

	if network != "unix" {
		fmt.Fprintf(os.Stderr, "COPILOT_AGENT_CONTROL_ADDR=%s\n", ln.Addr().String())
	}

	return func() {
		_ = controlServer.Close()
		_ = ln.Close()
		if network == "unix" {
			_ = os.Remove(socketPath)
		}
	}, nil
}

func StartClientLeaseWatcher(ctx context.Context, leaseDir string, stop func()) {
	leaseDir = strings.TrimSpace(leaseDir)
	if leaseDir == "" || stop == nil {
		return
	}
	log.Printf("[INFO] client lease watcher active dir=%s poll=%s grace=%s", leaseDir, clientLeasePollInterval, clientLeaseEmptyGrace)

	emptyThreshold := int(clientLeaseEmptyGrace / clientLeasePollInterval)
	if emptyThreshold < 1 {
		emptyThreshold = 1
	}

	go func() {
		ticker := time.NewTicker(clientLeasePollInterval)
		defer ticker.Stop()

		seenLease := false
		consecutiveEmpty := 0
		lastLeaseCount := -1
		shutdown := func(reason string) {
			log.Printf("[WARN] client lease dir empty (%s, %d consecutive checks); shutting down detached service", reason, consecutiveEmpty)
			stop()
		}

		check := func() (bool, string) {
			entries, err := os.ReadDir(leaseDir)
			if err != nil {
				if errors.Is(err, os.ErrNotExist) {
					if seenLease {
						consecutiveEmpty++
						if consecutiveEmpty >= emptyThreshold {
							return true, "missing"
						}
						return false, "missing"
					}
					return false, "missing"
				}
				log.Printf("[WARN] watch client leases %s: %v", leaseDir, err)
				consecutiveEmpty = 0
				return false, "error"
			}
			if count := len(entries); count != lastLeaseCount {
				log.Printf("[INFO] client lease watcher state dir=%s leases=%d seenLease=%t", leaseDir, count, seenLease)
				lastLeaseCount = count
			}
			if len(entries) > 0 {
				seenLease = true
				consecutiveEmpty = 0
				return false, "live"
			}
			if seenLease {
				consecutiveEmpty++
				if consecutiveEmpty >= emptyThreshold {
					return true, "empty"
				}
				return false, "empty"
			}
			return false, "empty"
		}

		if shouldStop, reason := check(); shouldStop {
			shutdown(reason)
			return
		}

		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				if shouldStop, reason := check(); shouldStop {
					shutdown(reason)
					return
				}
			}
		}
	}()
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	if w == nil {
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
