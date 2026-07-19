package main

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"claudexflow/internal/agorelay"
)

type config struct {
	database, listen, tlsCert, tlsKey, credentials string
	trustedProxies                                 []*net.IPNet
}

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := run(ctx, os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "ago-relay:", err)
		os.Exit(1)
	}
}

func parseConfig(args []string) (config, error) {
	flags := flag.NewFlagSet("ago-relay", flag.ContinueOnError)
	var value config
	var proxies string
	flags.StringVar(&value.database, "db", "", "relay SQLite database path")
	flags.StringVar(&value.listen, "listen", "127.0.0.1:8443", "HTTP listen address")
	flags.StringVar(&value.tlsCert, "tls-cert", "", "direct TLS certificate PEM")
	flags.StringVar(&value.tlsKey, "tls-key", "", "direct TLS private key PEM")
	flags.StringVar(&proxies, "trusted-proxies", "", "comma-separated trusted TLS-terminating proxy CIDRs")
	flags.StringVar(&value.credentials, "credentials", "", "optional private credential rotation JSON file")
	if err := flags.Parse(args); err != nil {
		return config{}, err
	}
	if flags.NArg() != 0 || strings.TrimSpace(value.database) == "" || strings.TrimSpace(value.listen) == "" {
		return config{}, errors.New("-db and -listen are required and positional arguments are not accepted")
	}
	direct := value.tlsCert != "" || value.tlsKey != ""
	forwarded := strings.TrimSpace(proxies) != ""
	if direct && (value.tlsCert == "" || value.tlsKey == "") {
		return config{}, errors.New("-tls-cert and -tls-key must be configured together")
	}
	if direct == forwarded {
		return config{}, errors.New("configure exactly one transport mode: direct TLS or trusted proxies")
	}
	if forwarded {
		for _, text := range strings.Split(proxies, ",") {
			_, network, err := net.ParseCIDR(strings.TrimSpace(text))
			if err != nil {
				return config{}, fmt.Errorf("invalid trusted proxy CIDR %q: %w", text, err)
			}
			value.trustedProxies = append(value.trustedProxies, network)
		}
	}
	return value, nil
}

func run(ctx context.Context, args []string) error {
	configuration, err := parseConfig(args)
	if err != nil {
		return err
	}
	store, err := agorelay.Open(configuration.database)
	if err != nil {
		return fmt.Errorf("open store: %w", err)
	}
	defer store.Close()
	if configuration.credentials != "" {
		if err := loadCredentials(ctx, store, configuration.credentials); err != nil {
			return err
		}
	}
	handler := agorelay.NewServer(store, agorelay.ServerConfig{TrustedProxies: configuration.trustedProxies}).Handler()
	server := &http.Server{
		Addr: configuration.listen, Handler: handler,
		ReadHeaderTimeout: 10 * time.Second, ReadTimeout: 45 * time.Second,
		WriteTimeout: 45 * time.Second, IdleTimeout: 90 * time.Second,
		MaxHeaderBytes: 32 << 10,
		TLSConfig:      &tls.Config{MinVersion: tls.VersionTLS12},
	}
	errCh := make(chan error, 1)
	go func() {
		if configuration.tlsCert != "" {
			errCh <- server.ListenAndServeTLS(configuration.tlsCert, configuration.tlsKey)
		} else {
			errCh <- server.ListenAndServe()
		}
	}()
	select {
	case err := <-errCh:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		return server.Shutdown(shutdownCtx)
	}
}

func loadCredentials(ctx context.Context, store *agorelay.Store, path string) error {
	linkInfo, err := os.Lstat(path)
	if err != nil {
		return fmt.Errorf("stat credentials: %w", err)
	}
	if linkInfo.Mode()&os.ModeSymlink != 0 || !linkInfo.Mode().IsRegular() || linkInfo.Mode().Perm()&0o077 != 0 {
		return errors.New("credentials file must be regular and private (mode 0600 or stricter)")
	}
	file, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("read credentials: %w", err)
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, (1<<20)+1))
	if err != nil {
		return fmt.Errorf("read credentials: %w", err)
	}
	if len(data) > 1<<20 {
		return errors.New("credentials file exceeds 1 MiB")
	}
	var credentials []agorelay.Credential
	decoder := json.NewDecoder(strings.NewReader(string(data)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&credentials); err != nil {
		return fmt.Errorf("decode credentials: %w", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return errors.New("credentials file must contain exactly one JSON value")
	}
	if len(credentials) == 0 || len(credentials) > 1024 {
		return errors.New("credentials file must contain 1..1024 credentials")
	}
	for _, credential := range credentials {
		if err := store.RotateCredential(ctx, credential); err != nil {
			return fmt.Errorf("rotate %s/%s/%s generation %d: %w", credential.AccountID, credential.DeviceID, credential.Role, credential.Generation, err)
		}
	}
	return nil
}
