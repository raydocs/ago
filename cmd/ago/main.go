package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"claudexflow/internal/agoattachments"
	"claudexflow/internal/agoauth"
	"claudexflow/internal/agobridge"
	"claudexflow/internal/agocoordinator"
	"claudexflow/internal/agodaemon"
	"claudexflow/internal/agogit"
	"claudexflow/internal/agolocalexec"
	"claudexflow/internal/agopluginhost"
	"claudexflow/internal/agopluginprotocol"
	"claudexflow/internal/agoprotocol"
	"claudexflow/internal/agothreadstore"
	"claudexflow/internal/agoverifier"
	"claudexflow/internal/agowritebroker"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "ago:", err)
		os.Exit(1)
	}
}

func run() error {
	mode, args := dispatch(os.Args[1:])
	if mode == "client" {
		ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
		defer stop()
		return runClient(ctx, args, os.Stdout, os.Stderr)
	}
	return runDaemon(args)
}

func dispatch(args []string) (string, []string) {
	if len(args) == 0 || strings.HasPrefix(args[0], "-") {
		return "daemon", args
	}
	if args[0] == "daemon" {
		return "daemon", args[1:]
	}
	return "client", args
}

func runDaemon(args []string) error {
	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	flags := flag.NewFlagSet("ago", flag.ContinueOnError)
	databasePath := flags.String("db", filepath.Join(home, ".local", "share", "ago", "ago.db"), "Ago SQLite database")
	attachmentsRoot := flags.String("attachments-root", "", "private attachment root (defaults to the database directory)")
	socketPath := flags.String("socket", filepath.Join(home, ".local", "state", "ago", "ago.sock"), "Ago daemon Unix socket")
	tcpListen := flags.String("tcp-listen", "", "optional numeric loopback TCP listen address")
	tcpEndpointFile := flags.String("tcp-endpoint-file", "", "private canonical path for the TCP endpoint descriptor")
	tcpBearerToken := flags.String("tcp-bearer-token", "", "high-entropy bearer token for loopback TCP HTTP")
	executorCommand := flags.String("executor-command", strings.TrimSpace(os.Getenv("AGO_EXECUTOR_COMMAND")), "JSONL sidecar command used for local turns")
	executorEntry := flags.String("executor-entry", strings.TrimSpace(os.Getenv("AGO_EXECUTOR_ENTRY")), "entrypoint passed to the JSONL sidecar command")
	supervisorCommand := flags.String("supervisor-command", strings.TrimSpace(os.Getenv("AGO_SUPERVISOR_COMMAND")), "Ago per-job supervisor executable")
	bunCommand := flags.String("bun", strings.TrimSpace(os.Getenv("AGO_BUN_COMMAND")), "Bun executable for trusted plugins")
	pluginRuntime := flags.String("plugin-runtime", envOrDefault("AGO_PLUGIN_RUNTIME", "plugin-runtime/main.ts"), "Ago trusted-plugin child entrypoint")
	pluginsConfig := flags.String("plugins-config", strings.TrimSpace(os.Getenv("AGO_PLUGINS_CONFIG")), "JSON file containing trusted workspace plugin configurations")
	bridgeRelayURL := flags.String("bridge-relay-url", strings.TrimSpace(os.Getenv("AGO_BRIDGE_RELAY_URL")), "optional outbound bridge HTTPS relay URL")
	bridgePin := flags.String("bridge-certificate-pin", strings.TrimSpace(os.Getenv("AGO_BRIDGE_CERTIFICATE_PIN")), "outbound bridge relay leaf-certificate SHA-256 pin")
	bridgeBearer := flags.String("bridge-bearer-token", strings.TrimSpace(os.Getenv("AGO_BRIDGE_BEARER_TOKEN")), "outbound bridge bearer credential")
	bridgeAccount := flags.String("bridge-account-id", strings.TrimSpace(os.Getenv("AGO_BRIDGE_ACCOUNT_ID")), "outbound bridge account identity")
	bridgeDevice := flags.String("bridge-device-id", strings.TrimSpace(os.Getenv("AGO_BRIDGE_DEVICE_ID")), "outbound bridge device identity")
	bridgeProjects := flags.String("bridge-allowed-projects", strings.TrimSpace(os.Getenv("AGO_BRIDGE_ALLOWED_PROJECTS")), "comma-separated outbound bridge project ACL")
	bridgePublications := flags.String("bridge-publications", strings.TrimSpace(os.Getenv("AGO_BRIDGE_PUBLICATIONS")), "JSON file containing explicit running-thread publications")
	bridgeStateRoot := flags.String("bridge-state-root", strings.TrimSpace(os.Getenv("AGO_BRIDGE_STATE_ROOT")), "private durable outbound bridge state directory")
	bridgePasskeyCredentials := flags.String("bridge-passkey-credentials", strings.TrimSpace(os.Getenv("AGO_BRIDGE_PASSKEY_CREDENTIALS")), "private persistent passkey credential file")
	bridgePasskeyRPID := flags.String("bridge-passkey-rp-id", strings.TrimSpace(os.Getenv("AGO_BRIDGE_PASSKEY_RP_ID")), "passkey relying-party ID for bridge mutations")
	bridgePasskeyOrigins := flags.String("bridge-passkey-origins", strings.TrimSpace(os.Getenv("AGO_BRIDGE_PASSKEY_ORIGINS")), "comma-separated HTTPS passkey origins")
	contextWindowTokens := flags.Int64("context-window-tokens", 1_050_000, "Ago-owned inference context window")
	reservedOutputTokens := flags.Int64("reserved-output-tokens", 32_000, "Ago-owned reserved inference output")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if *contextWindowTokens <= 0 || *reservedOutputTokens < 0 || *reservedOutputTokens >= *contextWindowTokens {
		return fmt.Errorf("context window must be positive and exceed the non-negative output reserve")
	}
	tcpConfig, err := loadOptionalTCPStartupConfig(tcpStartupFlags{Listen: *tcpListen, EndpointFile: *tcpEndpointFile, BearerToken: *tcpBearerToken})
	if err != nil {
		return err
	}
	bridgeConfig, err := loadOptionalBridgeStartupConfig(bridgeStartupFlags{
		RelayURL: *bridgeRelayURL, CertificatePin: *bridgePin, BearerToken: *bridgeBearer, AccountID: *bridgeAccount,
		DeviceID: *bridgeDevice, AllowedProjects: *bridgeProjects, PublicationsPath: *bridgePublications,
		StateRoot: *bridgeStateRoot, PasskeyCredentials: *bridgePasskeyCredentials, PasskeyRPID: *bridgePasskeyRPID,
		PasskeyOrigins: *bridgePasskeyOrigins,
	})
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(*databasePath), 0o700); err != nil {
		return fmt.Errorf("create database directory: %w", err)
	}
	store, err := agothreadstore.Open(*databasePath)
	if err != nil {
		return err
	}
	defer store.Close()
	attachmentStore, _, err := openProductionAttachmentStore(*databasePath, *attachmentsRoot)
	if err != nil {
		return err
	}
	defer attachmentStore.Close()

	listener, err := unixListener(*socketPath)
	if err != nil {
		return err
	}
	defer listener.Close()
	defer os.Remove(*socketPath)
	var tcpNetListener net.Listener
	if tcpConfig != nil {
		tcpNetListener, err = tcpListener(tcpConfig.listen)
		if err != nil {
			return err
		}
		defer tcpNetListener.Close()
	}

	if strings.TrimSpace(*supervisorCommand) == "" {
		current, err := os.Executable()
		if err != nil {
			return fmt.Errorf("resolve Ago executable: %w", err)
		}
		*supervisorCommand = filepath.Join(filepath.Dir(current), "ago-supervisor")
	}
	supervisorPath, err := resolveExecutable(*supervisorCommand)
	if err != nil {
		return fmt.Errorf("resolve local supervisor: %w", err)
	}
	verifier, err := newProductionVerifier(store, supervisorPath)
	if err != nil {
		return fmt.Errorf("configure production verification: %w", err)
	}

	var executor agocoordinator.Executor = agolocalexec.BrokerExecutor{}
	var classifierExecutor agocoordinator.SessionExecutor
	if strings.TrimSpace(*executorCommand) != "" {
		executorPath, err := resolveExecutable(*executorCommand)
		if err != nil {
			return fmt.Errorf("resolve local executor: %w", err)
		}
		arguments := []string(nil)
		readRoots := []string(nil)
		entryPath := ""
		if strings.TrimSpace(*executorEntry) != "" {
			entry, err := filepath.Abs(*executorEntry)
			if err != nil {
				return fmt.Errorf("resolve local executor entry: %w", err)
			}
			entry, err = filepath.EvalSymlinks(entry)
			if err != nil {
				return fmt.Errorf("resolve local executor entry: %w", err)
			}
			entryPath = entry
			readRoot, err := executorReadRoot(entry)
			if err != nil {
				return fmt.Errorf("resolve local executor package: %w", err)
			}
			arguments = executorArguments(entry, readRoot)
			readRoots = []string{readRoot}
		}
		provider, model := initialInferenceRoute()
		providerBridge := trustedProviderProcess{}
		if len(arguments) == 0 {
			// Fixture sidecars that do not perform inference need no provider broker.
		} else {
			providerEntry := filepath.Join(filepath.Dir(entryPath), "provider-process.ts")
			if info, statErr := os.Stat(providerEntry); statErr == nil && !info.IsDir() {
				providerBridge = trustedProviderProcess{
					Command: executorPath, Arguments: []string{providerEntry}, Provider: provider, Model: model,
					Environment: trustedProviderEnvironment(provider),
				}
			}
		}
		broker := agolocalexec.BrokerExecutor{
			Supervisor: supervisorPath, Command: executorPath, Arguments: arguments, ReadRoots: readRoots,
			Protocol: "pi-jsonl-v1", Provider: provider, Model: model, ProviderCallback: providerBridge.Callback,
		}
		executor = broker
		classifierExecutor = broker
	}
	if *bunCommand == "" {
		*bunCommand, err = exec.LookPath("bun")
		if err != nil {
			return fmt.Errorf("find Bun for trusted plugins: %w", err)
		}
	}
	pluginEntry, err := filepath.Abs(*pluginRuntime)
	if err != nil {
		return fmt.Errorf("resolve plugin runtime: %w", err)
	}
	dialogs := newDialogBroker(store)
	if err := dialogs.RecoverStaleDialogs(context.Background()); err != nil {
		return fmt.Errorf("recover stale plugin dialogs: %w", err)
	}
	configuredPlugins, err := loadPluginConfigs(*pluginsConfig)
	if err != nil {
		return err
	}
	classifier := piAIClassifier{store: store, executor: classifierExecutor}
	pluginRegistry := agopluginhost.NewWorkspaceRegistry(func(workspace string) *agopluginhost.Manager {
		var manager *agopluginhost.Manager
		manager = agopluginhost.NewManager(agopluginhost.NewProcessFactory(*bunCommand, pluginEntry, agopluginhost.ProcessOptions{
			MaxMessageBytes: 1 << 20, ExitGrace: 5 * time.Second,
			AIAsk: classifier.Ask,
			UI: func(ctx context.Context, params agopluginprotocol.UIRequestParams) agopluginprotocol.UIResult {
				return dialogs.Request(ctx, manager, params)
			},
		}), 2*time.Second)
		manager.SetGenerationRetirer(func(generation int64, reason string) {
			dialogs.RetireWorkspaceGeneration(workspace, generation, reason)
		})
		return manager
	}, agopluginhost.ReloadConfig{
		Plugins: configuredPlugins,
		Capabilities: agopluginprotocol.Capabilities{UI: []agopluginprotocol.UIKind{
			agopluginprotocol.UINotify, agopluginprotocol.UIConfirm, agopluginprotocol.UIInput, agopluginprotocol.UISelect,
		}, RenderMode: "client-neutral"},
		Limits: agopluginprotocol.Limits{MaxMessageBytes: 1 << 20, MaxInflight: 64},
	})
	defer pluginRegistry.Shutdown(context.Background())
	preparer := automaticContextPreparer{
		store: store,
		compactor: agothreadstore.NewCompactor(store, agothreadstore.CompactionBudget{
			ContextWindowTokens: *contextWindowTokens, ReservedOutputTokens: *reservedOutputTokens, TriggerRatio: 0.90,
		}),
	}
	tools := productionTools{store: store, plugins: pluginRegistry, verifier: verifier}
	coordinator := agocoordinator.NewRuntime(store, executor, tools, preparer)
	if err := coordinator.Recover(context.Background()); err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	bridge, err := startOptionalOutboundBridge(ctx, bridgeConfig, store, coordinator)
	if err != nil {
		return fmt.Errorf("start outbound bridge: %w", err)
	}
	if bridge != nil {
		defer bridge.Shutdown(context.Background())
	}
	baseHandler := agodaemon.NewWithRuntime(store, coordinator, dialogs, tools).WithGitRefresher(agogit.NewService(store)).WithAttachments(attachmentStore).Handler()
	unixServer := &http.Server{Handler: baseHandler, ReadHeaderTimeout: 5 * time.Second}
	type serveResult struct {
		transport string
		err       error
	}
	result := make(chan serveResult, 2)
	go func() { result <- serveResult{transport: "Unix", err: unixServer.Serve(listener)} }()
	var tcpServer *http.Server
	if tcpConfig != nil {
		authenticated, err := agodaemon.RequireBearerToken(baseHandler, tcpConfig.bearerToken)
		if err != nil {
			return err
		}
		tcpServer = &http.Server{Handler: authenticated, ReadHeaderTimeout: 5 * time.Second}
		if _, err := writeTCPEndpoint(tcpConfig.endpointFile, tcpNetListener.Addr()); err != nil {
			return err
		}
		defer func() { _ = removeTCPEndpoint(tcpConfig.endpointFile) }()
		go func() { result <- serveResult{transport: "TCP", err: tcpServer.Serve(tcpNetListener)} }()
	}
	var bridgeErrors <-chan error
	if bridge != nil {
		bridgeErrors = bridge.Errors()
	}
	select {
	case stopped := <-result:
		if stopped.err == nil {
			return fmt.Errorf("%s HTTP server stopped unexpectedly", stopped.transport)
		}
		return fmt.Errorf("%s HTTP server stopped unexpectedly: %w", stopped.transport, stopped.err)
	case err := <-bridgeErrors:
		if err == nil {
			return errors.New("outbound bridge stopped unexpectedly")
		}
		return fmt.Errorf("outbound bridge: %w", err)
	case <-ctx.Done():
		shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := unixServer.Shutdown(shutdown); err != nil {
			return err
		}
		if tcpServer != nil {
			if err := tcpServer.Shutdown(shutdown); err != nil {
				return err
			}
			if err := removeTCPEndpoint(tcpConfig.endpointFile); err != nil {
				return err
			}
		}
		if bridge != nil {
			if err := bridge.Shutdown(shutdown); err != nil {
				return err
			}
		}
		if err := coordinator.Shutdown(shutdown); err != nil {
			return err
		}
		return pluginRegistry.Shutdown(shutdown)
	}
}

type bridgeStartupFlags struct {
	RelayURL, CertificatePin, BearerToken, AccountID, DeviceID string
	AllowedProjects, PublicationsPath, StateRoot               string
	PasskeyCredentials, PasskeyRPID, PasskeyOrigins            string
}

type bridgeStartupConfig struct {
	client             agobridge.Config
	publications       agodaemon.BridgePublicationConfig
	stateRoot          string
	passkeyCredentials string
	passkeyRPID        string
	passkeyOrigins     []string
}

func loadOptionalBridgeStartupConfig(input bridgeStartupFlags) (*bridgeStartupConfig, error) {
	values := []string{input.RelayURL, input.CertificatePin, input.BearerToken, input.AccountID, input.DeviceID, input.AllowedProjects,
		input.PublicationsPath, input.StateRoot, input.PasskeyCredentials, input.PasskeyRPID, input.PasskeyOrigins}
	configured := 0
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			configured++
		}
	}
	if configured == 0 {
		return nil, nil
	}
	if configured != len(values) {
		return nil, errors.New("outbound bridge configuration is partial; every bridge and passkey setting is required")
	}
	projects, err := strictCSV(input.AllowedProjects)
	if err != nil {
		return nil, fmt.Errorf("bridge allowed projects: %w", err)
	}
	origins, err := strictCSV(input.PasskeyOrigins)
	if err != nil {
		return nil, fmt.Errorf("bridge passkey origins: %w", err)
	}
	publicationBytes, err := os.ReadFile(input.PublicationsPath)
	if err != nil {
		return nil, fmt.Errorf("read bridge publications: %w", err)
	}
	var publications agodaemon.BridgePublicationConfig
	decoder := json.NewDecoder(bytes.NewReader(publicationBytes))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&publications); err != nil {
		return nil, fmt.Errorf("decode bridge publications: %w", err)
	}
	if decoder.Decode(&struct{}{}) != io.EOF {
		return nil, errors.New("bridge publications contain trailing JSON")
	}
	allowed := make(map[string]struct{}, len(projects))
	for _, project := range projects {
		allowed[project] = struct{}{}
	}
	for _, project := range agodaemon.SortedBridgeProjects(publications) {
		if _, ok := allowed[project]; !ok {
			return nil, fmt.Errorf("published project %q is outside bridge ACL", project)
		}
	}
	return &bridgeStartupConfig{
		client: agobridge.Config{RelayURL: input.RelayURL, CertificatePin: input.CertificatePin, BearerToken: input.BearerToken,
			AccountID: input.AccountID, DeviceID: input.DeviceID, AllowedProjects: allowed},
		publications: publications, stateRoot: input.StateRoot, passkeyCredentials: input.PasskeyCredentials,
		passkeyRPID: input.PasskeyRPID, passkeyOrigins: origins,
	}, nil
}

func strictCSV(value string) ([]string, error) {
	parts := strings.Split(value, ",")
	seen := make(map[string]struct{}, len(parts))
	for _, part := range parts {
		if part == "" || strings.TrimSpace(part) != part {
			return nil, errors.New("entries must be non-empty and whitespace-free")
		}
		if _, exists := seen[part]; exists {
			return nil, errors.New("entries must be unique")
		}
		seen[part] = struct{}{}
	}
	return parts, nil
}

type wallClock struct{}

func (wallClock) Now() time.Time { return time.Now() }

func startOptionalOutboundBridge(ctx context.Context, config *bridgeStartupConfig, store *agothreadstore.Store, coordinator *agocoordinator.Coordinator) (*agodaemon.OutboundBridge, error) {
	if config == nil {
		return nil, nil
	}
	stateStore, err := agobridge.NewFileStateStore(config.stateRoot)
	if err != nil {
		return nil, err
	}
	credentialStore, err := agoauth.NewFileCredentialPersistence(config.passkeyCredentials)
	if err != nil {
		stateStore.Close()
		return nil, err
	}
	auth, err := agoauth.New(agoauth.Config{
		RelyingParties: map[string][]string{config.passkeyRPID: config.passkeyOrigins}, ChallengeTTL: 5 * time.Minute,
		GrantTTL: 5 * time.Minute, ChallengeBytes: 32, MaxChallenges: 256, MaxGrants: 256, RequireUserVerification: true,
	}, agoauth.Dependencies{Clock: wallClock{}, Random: rand.Reader, Verifier: agodaemon.StandardPasskeyVerifier{}, Persistence: credentialStore})
	if err != nil {
		stateStore.Close()
		return nil, err
	}
	config.client.StateStore = stateStore
	bridge, err := agodaemon.StartOutboundBridge(ctx, agodaemon.OutboundBridgeConfig{
		Client: config.client, Publications: config.publications, Store: store, Coordinator: coordinator,
		Authorization: agodaemon.RecentPasskeyAuthorization{Core: auth}, Closer: stateStore,
	})
	if err != nil {
		stateStore.Close()
		return nil, err
	}
	return bridge, nil
}

func openProductionAttachmentStore(databasePath, configuredRoot string) (*agoattachments.Store, string, error) {
	root := strings.TrimSpace(configuredRoot)
	if root == "" {
		root = filepath.Dir(databasePath)
	}
	if !filepath.IsAbs(root) {
		absolute, err := filepath.Abs(root)
		if err != nil {
			return nil, "", fmt.Errorf("resolve attachment root: %w", err)
		}
		root = absolute
	}
	if err := os.MkdirAll(root, 0o700); err != nil {
		return nil, "", fmt.Errorf("create attachment root: %w", err)
	}
	store, err := agoattachments.Open(root)
	if err != nil {
		return nil, "", fmt.Errorf("open attachment store: %w", err)
	}
	return store, root, nil
}

func loadPluginConfigs(path string) ([]agopluginprotocol.PluginConfig, error) {
	if strings.TrimSpace(path) == "" {
		return nil, nil
	}
	canonical, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	data, err := os.ReadFile(canonical)
	if err != nil {
		return nil, fmt.Errorf("read trusted plugin config: %w", err)
	}
	if len(data) > 1<<20 {
		return nil, fmt.Errorf("trusted plugin config exceeds 1 MiB")
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	var plugins []agopluginprotocol.PluginConfig
	if err := decoder.Decode(&plugins); err != nil {
		return nil, fmt.Errorf("decode trusted plugin config: %w", err)
	}
	var extra any
	if decoder.Decode(&extra) != io.EOF {
		return nil, fmt.Errorf("trusted plugin config must contain one JSON array")
	}
	for _, plugin := range plugins {
		if strings.TrimSpace(plugin.PluginID) == "" || strings.TrimSpace(plugin.EntryURI) == "" {
			return nil, fmt.Errorf("trusted plugin ID and entry URI are required")
		}
	}
	return plugins, nil
}

type productionTools struct {
	store    *agothreadstore.Store
	plugins  *agopluginhost.WorkspaceRegistry
	verifier *agoverifier.Service
}

const (
	productionVerificationToolName = "ago_verify"
	productionGoTestCheckID        = "go-test"
)

func productionVerificationCatalog(goExecutable string) agoverifier.StaticCatalog {
	return agoverifier.StaticCatalog{
		productionGoTestCheckID: {Executable: goExecutable, Args: []string{"test", "./..."}, Timeout: 2 * time.Minute},
	}
}

func productionVerificationTool() agocoordinator.ExternalTool {
	return agocoordinator.ExternalTool{
		Name:        productionVerificationToolName,
		Description: "Run one server-owned, sandboxed verification check and return its durable ledger record",
		InputSchema: json.RawMessage(`{"type":"object","properties":{"check_id":{"type":"string","enum":["go-test"]}},"required":["check_id"],"additionalProperties":false}`),
	}
}

func newProductionVerifier(store *agothreadstore.Store, supervisor string) (*agoverifier.Service, error) {
	if store == nil {
		return nil, fmt.Errorf("thread store is required")
	}
	goExecutable, err := resolveExecutable("go")
	if err != nil {
		return nil, fmt.Errorf("resolve Go verification executable: %w", err)
	}
	goEnvironment, readRoots, err := productionGoEnvironment(goExecutable)
	if err != nil {
		return nil, err
	}
	executor := agoverifier.SeatbeltExecutor{Supervisor: supervisor, ReadRoots: readRoots, Environment: goEnvironment}
	return agoverifier.New(store, store, productionVerificationCatalog(goExecutable), executor, agoverifier.Limits{
		DefaultTimeout: 2 * time.Minute, MaxTimeout: 2 * time.Minute, MaxOutputBytes: 8 << 10,
	}), nil
}

func productionGoEnvironment(goExecutable string) (map[string]string, []string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, goExecutable, "env", "-json", "GOROOT", "GOMODCACHE")
	var stdout, stderr boundedProviderLog
	command.Stdout, command.Stderr = &stdout, &stderr
	if err := command.Run(); err != nil {
		return nil, nil, fmt.Errorf("read Go verification environment: %w (%s)", err, stderr.String())
	}
	var values struct {
		GOROOT     string
		GOMODCACHE string
	}
	decoder := json.NewDecoder(bytes.NewReader(stdout.Bytes()))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&values); err != nil || values.GOROOT == "" || values.GOMODCACHE == "" {
		return nil, nil, fmt.Errorf("decode Go verification environment: %w", err)
	}
	readRoots := make([]string, 0, 2)
	for _, root := range []string{values.GOROOT, values.GOMODCACHE} {
		canonical, err := filepath.EvalSymlinks(root)
		if err != nil || !filepath.IsAbs(canonical) || filepath.Clean(canonical) != canonical {
			return nil, nil, fmt.Errorf("canonicalize Go verification read root %q: %w", root, err)
		}
		readRoots = append(readRoots, canonical)
	}
	return map[string]string{
		"GOROOT": readRoots[0], "GOMODCACHE": readRoots[1], "GOENV": "off",
		"GOPROXY": "off", "GOSUMDB": "off", "GOTOOLCHAIN": "local",
	}, readRoots, nil
}

type piAIClassifier struct {
	store    *agothreadstore.Store
	executor agocoordinator.SessionExecutor
}

func executorReadRoot(entry string) (string, error) {
	sourceRoot := filepath.Dir(entry)
	providerEntry := filepath.Join(sourceRoot, "provider-process.ts")
	providerInfo, err := os.Stat(providerEntry)
	if os.IsNotExist(err) {
		return sourceRoot, nil
	}
	if err != nil {
		return "", err
	}
	if providerInfo.IsDir() {
		return "", fmt.Errorf("provider entry is a directory")
	}
	packageRoot := filepath.Dir(sourceRoot)
	manifest, err := os.Stat(filepath.Join(packageRoot, "package.json"))
	if err != nil {
		return "", fmt.Errorf("read package manifest: %w", err)
	}
	if manifest.IsDir() {
		return "", fmt.Errorf("package manifest is a directory")
	}
	dependencies, err := os.Stat(filepath.Join(packageRoot, "node_modules"))
	if err != nil {
		return "", fmt.Errorf("read package dependencies: %w", err)
	}
	if !dependencies.IsDir() {
		return "", fmt.Errorf("package dependencies are not a directory")
	}
	return packageRoot, nil
}

func executorArguments(entry, readRoot string) []string {
	if readRoot != filepath.Dir(entry) {
		return []string{"run", "--cwd", readRoot, entry}
	}
	return []string{entry}
}

type trustedProviderProcess struct {
	Command     string
	Arguments   []string
	Provider    string
	Model       string
	Environment []string
}

func (provider trustedProviderProcess) Callback(ctx context.Context, request agolocalexec.ProviderRequest, emit func(agolocalexec.ProviderResponse) error) error {
	if provider.Command == "" {
		return fmt.Errorf("trusted provider process is unavailable")
	}
	if request.Type != "inference_request" || request.Provider != provider.Provider || request.Model != provider.Model {
		return fmt.Errorf("sandbox requested a non-authoritative provider route")
	}
	encoded, err := json.Marshal(request)
	if err != nil || len(encoded) > 1<<20 {
		return fmt.Errorf("encode trusted provider request")
	}
	processCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	command := exec.CommandContext(processCtx, provider.Command, provider.Arguments...)
	command.Env = append([]string(nil), provider.Environment...)
	command.Stdin = bytes.NewReader(append(encoded, '\n'))
	stdout, err := command.StdoutPipe()
	if err != nil {
		return err
	}
	var stderr boundedProviderLog
	command.Stderr = &stderr
	if err := command.Start(); err != nil {
		return fmt.Errorf("start trusted provider process: %w", err)
	}
	terminal := false
	scanner := bufio.NewScanner(stdout)
	scanner.Buffer(make([]byte, 64<<10), 1<<20)
	for scanner.Scan() {
		var response agolocalexec.ProviderResponse
		decoder := json.NewDecoder(bytes.NewReader(scanner.Bytes()))
		decoder.DisallowUnknownFields()
		if decoder.Decode(&response) != nil || decoder.Decode(&struct{}{}) != io.EOF || response.ID != request.ID {
			cancel()
			_ = command.Wait()
			return fmt.Errorf("trusted provider returned an invalid frame")
		}
		valid := false
		switch response.Type {
		case "delta":
			valid = !terminal && response.Delta != "" && len(response.Message) == 0 && response.Error == ""
		case "result":
			valid = !terminal && response.Delta == "" && len(response.Message) > 0 && json.Valid(response.Message) && response.Error == ""
			terminal = valid
		case "error":
			valid = !terminal && response.Delta == "" && len(response.Message) == 0 && response.Error != ""
			terminal = valid
		}
		if !valid || emit(response) != nil {
			cancel()
			_ = command.Wait()
			return fmt.Errorf("trusted provider returned an invalid response sequence")
		}
	}
	if err := scanner.Err(); err != nil {
		cancel()
		_ = command.Wait()
		return fmt.Errorf("read trusted provider response: %w", err)
	}
	if err := command.Wait(); err != nil {
		return fmt.Errorf("trusted provider process failed: %w (%s)", err, stderr.String())
	}
	if !terminal {
		return fmt.Errorf("trusted provider ended without a terminal response")
	}
	return nil
}

type boundedProviderLog struct{ bytes.Buffer }

func (log *boundedProviderLog) Write(value []byte) (int, error) {
	original := len(value)
	if remaining := (64 << 10) - log.Len(); remaining > 0 {
		if len(value) > remaining {
			value = value[:remaining]
		}
		_, _ = log.Buffer.Write(value)
	}
	return original, nil
}

func trustedProviderEnvironment(provider string) []string {
	environment := []string{"PATH=/usr/bin:/bin:/usr/sbin:/sbin"}
	if provider == "openai" {
		if key := strings.TrimSpace(os.Getenv("OPENAI_API_KEY")); key != "" {
			environment = append(environment, "OPENAI_API_KEY="+key)
		}
	}
	return environment
}

func (classifier piAIClassifier) Ask(ctx context.Context, params agopluginprotocol.AIAskParams) (agopluginprotocol.AIAskResult, error) {
	if classifier.store == nil || classifier.executor == nil {
		return agopluginprotocol.AIAskResult{}, fmt.Errorf("Ago classifier is unavailable")
	}
	thread, err := classifier.store.Thread(ctx, params.ThreadID)
	if err != nil {
		return agopluginprotocol.AIAskResult{}, err
	}
	requestJSON, err := json.Marshal(struct {
		Question string   `json:"question"`
		Context  string   `json:"context,omitempty"`
		Options  []string `json:"options,omitempty"`
	}{Question: params.Question, Context: params.Context, Options: params.Options})
	if err != nil {
		return agopluginprotocol.AIAskResult{}, err
	}
	prompt := "Classify the following request. Return exactly one JSON object and no markdown or extra text. " +
		`Required schema: {"answer":"yes|no|uncertain","probability":number from 0 to 1,"reason":"non-empty concise reason"}. Request: ` + string(requestJSON)
	content, _ := json.Marshal(map[string]string{"text": prompt})
	sessionCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	execution, err := classifier.executor.Start(sessionCtx, agocoordinator.TurnRequest{
		ThreadID: params.ThreadID, TurnID: params.TurnID + ":classifier:" + params.InvocationID,
		Attempt: 1, Content: content, Workspace: thread.Workspace, Mode: agoprotocol.AgentModeMedium,
		Executor: agoprotocol.ExecutorTarget{Type: agoprotocol.ExecutorLocal}, Tools: nil,
	})
	if err != nil {
		return agopluginprotocol.AIAskResult{}, fmt.Errorf("start dedicated classifier session: %w", err)
	}
	defer execution.CloseInput()
	var output bytes.Buffer
	for event := range execution.Events() {
		if event.Type != "text" {
			continue
		}
		var frame struct {
			Type  string `json:"type"`
			Delta string `json:"delta"`
		}
		decoder := json.NewDecoder(bytes.NewReader(event.Payload))
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&frame); err != nil || frame.Type != "text" || decoder.Decode(&struct{}{}) != io.EOF {
			return agopluginprotocol.AIAskResult{}, fmt.Errorf("invalid classifier text event")
		}
		if output.Len()+len(frame.Delta) > 8<<10 {
			return agopluginprotocol.AIAskResult{}, fmt.Errorf("classifier result exceeds 8 KiB")
		}
		_, _ = output.WriteString(frame.Delta)
	}
	if err := execution.CloseInput(); err != nil {
		return agopluginprotocol.AIAskResult{}, fmt.Errorf("close dedicated classifier input: %w", err)
	}
	if err := execution.Wait(); err != nil {
		return agopluginprotocol.AIAskResult{}, fmt.Errorf("dedicated classifier session: %w", err)
	}
	result, err := agopluginprotocol.DecodeAIAskResult(output.Bytes())
	if err != nil {
		return agopluginprotocol.AIAskResult{}, fmt.Errorf("invalid classifier result: %w", err)
	}
	return result, nil
}

type automaticContextPreparer struct {
	store     *agothreadstore.Store
	compactor *agothreadstore.Compactor
}

type dialogBroker struct {
	store   *agothreadstore.Store
	mu      sync.Mutex
	waiters map[string]chan agopluginprotocol.UIResult
}

func newDialogBroker(store *agothreadstore.Store) *dialogBroker {
	return &dialogBroker{store: store, waiters: make(map[string]chan agopluginprotocol.UIResult)}
}

func (broker *dialogBroker) Request(ctx context.Context, plugins *agopluginhost.Manager, params agopluginprotocol.UIRequestParams) agopluginprotocol.UIResult {
	correlation, found := plugins.Invocation(params.InvocationID)
	if !found || params.Generation < 0 {
		return agopluginprotocol.HeadlessUIResult(params.Request.Kind)
	}
	request, err := json.Marshal(params.Request)
	if err != nil {
		return agopluginprotocol.UIResult{Status: agopluginprotocol.UIStatusUnavailable}
	}
	deadline := time.UnixMilli(params.DeadlineUnixMs)
	if params.DeadlineUnixMs <= 0 {
		deadline = time.Now().Add(2 * time.Minute)
	}
	dialog, err := broker.store.CreatePendingDialog(ctx, agothreadstore.CreateDialogInput{
		ThreadID: correlation.ThreadID, TurnID: correlation.TurnID, PluginID: params.PluginID,
		Generation: uint64(params.Generation), InvocationID: params.InvocationID, Deadline: deadline,
		RequestType: string(params.Request.Kind), Request: request,
	})
	if err != nil {
		return agopluginprotocol.UIResult{Status: agopluginprotocol.UIStatusUnavailable}
	}
	if params.Request.Kind == agopluginprotocol.UINotify {
		result := agopluginprotocol.UIResult{Status: agopluginprotocol.UIStatusOK}
		encoded, _ := json.Marshal(result)
		_, _ = broker.ResolveDialog(context.Background(), agothreadstore.ResolveDialogInput{DialogID: dialog.DialogID, ResolverID: "ago-notification", ExpectedRevision: dialog.Revision, Response: encoded})
		return result
	}
	waiter := make(chan agopluginprotocol.UIResult, 1)
	broker.mu.Lock()
	broker.waiters[dialog.DialogID] = waiter
	broker.mu.Unlock()
	defer func() {
		broker.mu.Lock()
		delete(broker.waiters, dialog.DialogID)
		broker.mu.Unlock()
	}()
	if all, listErr := broker.store.ListDialogs(context.Background(), dialog.ThreadID); listErr == nil {
		for _, current := range all {
			if current.DialogID == dialog.DialogID && current.State == agothreadstore.DialogResolved {
				var result agopluginprotocol.UIResult
				if json.Unmarshal(current.Response, &result) == nil {
					return result
				}
			}
		}
	}
	select {
	case result := <-waiter:
		return result
	case <-ctx.Done():
		result := agopluginprotocol.UIResult{Status: agopluginprotocol.UIStatusTimeout}
		encoded, _ := json.Marshal(result)
		_, _ = broker.ResolveDialog(context.Background(), agothreadstore.ResolveDialogInput{DialogID: dialog.DialogID, ResolverID: "ago-deadline", ExpectedRevision: dialog.Revision, Response: encoded})
		return result
	}
}

func (broker *dialogBroker) ResolveDialog(ctx context.Context, input agothreadstore.ResolveDialogInput) (agothreadstore.PluginDialog, error) {
	var result agopluginprotocol.UIResult
	decoder := json.NewDecoder(bytes.NewReader(input.Response))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&result); err != nil {
		return agothreadstore.PluginDialog{}, fmt.Errorf("invalid plugin UI result: %w", err)
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return agothreadstore.PluginDialog{}, fmt.Errorf("plugin UI result must contain exactly one JSON value")
	}
	dialog, err := broker.store.Dialog(ctx, input.DialogID)
	if err != nil {
		return agothreadstore.PluginDialog{}, err
	}
	switch result.Status {
	case agopluginprotocol.UIStatusOK:
		if err := validateUIValue(dialog.RequestType, result.Value); err != nil {
			return agothreadstore.PluginDialog{}, err
		}
	case agopluginprotocol.UIStatusCancelled, agopluginprotocol.UIStatusUnavailable, agopluginprotocol.UIStatusTimeout:
		if len(result.Value) != 0 {
			return agothreadstore.PluginDialog{}, fmt.Errorf("plugin UI status %q must not include a value", result.Status)
		}
	default:
		return agothreadstore.PluginDialog{}, fmt.Errorf("invalid plugin UI status %q", result.Status)
	}
	dialog, err = broker.store.ResolveDialog(ctx, input)
	if err != nil {
		return agothreadstore.PluginDialog{}, err
	}
	broker.mu.Lock()
	waiter := broker.waiters[dialog.DialogID]
	broker.mu.Unlock()
	if waiter != nil {
		select {
		case waiter <- result:
		default:
		}
	}
	return dialog, nil
}

func (broker *dialogBroker) RetireGeneration(generation int64, _ string) {
	if generation < 0 {
		return
	}
	dialogs, err := broker.store.ListPendingDialogsByGeneration(context.Background(), uint64(generation))
	if err != nil {
		return
	}
	response, _ := json.Marshal(agopluginprotocol.UIResult{Status: agopluginprotocol.UIStatusCancelled})
	for _, dialog := range dialogs {
		_, _ = broker.ResolveDialog(context.Background(), agothreadstore.ResolveDialogInput{
			DialogID: dialog.DialogID, ResolverID: "ago-generation-retired", ExpectedRevision: dialog.Revision, Response: response,
		})
	}
}

func (broker *dialogBroker) RetireWorkspaceGeneration(workspace string, generation int64, _ string) {
	if generation < 0 {
		return
	}
	dialogs, err := broker.store.ListPendingDialogsByGeneration(context.Background(), uint64(generation))
	if err != nil {
		return
	}
	response, _ := json.Marshal(agopluginprotocol.UIResult{Status: agopluginprotocol.UIStatusCancelled})
	for _, dialog := range dialogs {
		thread, threadErr := broker.store.Thread(context.Background(), dialog.ThreadID)
		if threadErr != nil {
			continue
		}
		canonical, canonicalErr := filepath.EvalSymlinks(thread.Workspace)
		if canonicalErr != nil || canonical != workspace {
			continue
		}
		_, _ = broker.ResolveDialog(context.Background(), agothreadstore.ResolveDialogInput{
			DialogID: dialog.DialogID, ResolverID: "ago-generation-retired", ExpectedRevision: dialog.Revision, Response: response,
		})
	}
}

func (broker *dialogBroker) RecoverStaleDialogs(ctx context.Context) error {
	dialogs, err := broker.store.ListAllPendingDialogs(ctx)
	if err != nil {
		return err
	}
	response, _ := json.Marshal(agopluginprotocol.UIResult{Status: agopluginprotocol.UIStatusUnavailable})
	for _, dialog := range dialogs {
		if _, err := broker.ResolveDialog(ctx, agothreadstore.ResolveDialogInput{
			DialogID: dialog.DialogID, ResolverID: "ago-daemon-recovery", ExpectedRevision: dialog.Revision, Response: response,
		}); err != nil {
			return err
		}
	}
	return nil
}

func validateUIValue(kind string, value json.RawMessage) error {
	switch agopluginprotocol.UIKind(kind) {
	case agopluginprotocol.UINotify:
		if len(value) != 0 {
			return fmt.Errorf("notification result must not include a value")
		}
		return nil
	case agopluginprotocol.UIConfirm:
		var confirmed any
		if len(value) == 0 || json.Unmarshal(value, &confirmed) != nil {
			return fmt.Errorf("confirmation result value must be a boolean")
		}
		if _, ok := confirmed.(bool); !ok {
			return fmt.Errorf("confirmation result value must be a boolean")
		}
		return nil
	case agopluginprotocol.UIInput, agopluginprotocol.UISelect:
		var selected string
		if len(value) == 0 || json.Unmarshal(value, &selected) != nil {
			return fmt.Errorf("%s result value must be a string", kind)
		}
		return nil
	default:
		return fmt.Errorf("unsupported plugin UI request type %q", kind)
	}
}

func (preparer automaticContextPreparer) PrepareContext(ctx context.Context, request agocoordinator.TurnRequest) (agothreadstore.ContextProjection, error) {
	encoded, err := json.Marshal(struct {
		Context agothreadstore.ContextProjection `json:"context"`
		Prompt  json.RawMessage                  `json:"prompt"`
	}{request.Context, request.Content})
	if err != nil {
		return agothreadstore.ContextProjection{}, err
	}
	var currentSequence uint64
	for _, event := range request.Context.Tail {
		if event.Type != agoprotocol.EventMessageAccepted {
			continue
		}
		var payload struct {
			TurnID string `json:"turn_id"`
		}
		if json.Unmarshal(event.Payload, &payload) == nil && payload.TurnID == request.TurnID {
			currentSequence = event.Sequence
		}
	}
	if currentSequence == 0 {
		return agothreadstore.ContextProjection{}, fmt.Errorf("current accepted message is absent from context projection")
	}
	if currentSequence > 1 {
		_, _, err = preparer.compactor.MaybeCompactThrough(ctx, request.ThreadID, currentSequence-1, int64((len(encoded)+3)/4), deterministicRecoverySummary)
		if err != nil {
			return agothreadstore.ContextProjection{}, err
		}
	}
	return preparer.store.ContextProjection(ctx, request.ThreadID)
}

func deterministicRecoverySummary(projection agothreadstore.ContextProjection) (string, error) {
	encoded, err := json.Marshal(projection)
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256(encoded)
	capsule := recoveryCapsule{Version: 2, SourceSHA256: fmt.Sprintf("%x", digest)}
	sections := []*recoverySection{&capsule.Objective, &capsule.AcceptanceCriteria, &capsule.Decisions, &capsule.ChangedPaths, &capsule.Verification, &capsule.ActiveWork, &capsule.UnresolvedIssues, &capsule.NextAction}
	for _, section := range sections {
		section.Evidence = []recoveryEvidence{}
	}
	capsule.Objective.Status = "source-events-require-interpretation"
	capsule.AcceptanceCriteria.Status = "source-events-require-interpretation"
	capsule.Decisions.Status = "source-events-require-interpretation"
	capsule.ChangedPaths.Status = "not-explicitly-recorded"
	capsule.Verification.Status = "source-events-require-interpretation"
	capsule.ActiveWork.Status = "recent-authoritative-events"
	capsule.UnresolvedIssues.Status = "no-unprepared-tool-request-observed"
	capsule.NextAction.Status = "latest-accepted-user-message"
	if projection.Compaction != nil {
		capsule.PreviousSummary = projection.Compaction.Summary
		if len(capsule.PreviousSummary) > 8<<10 {
			capsule.PreviousSummary = capsule.PreviousSummary[len(capsule.PreviousSummary)-(8<<10):]
		}
	}
	requested := make(map[string]recoveryEvidence)
	prepared := make(map[string]bool)
	for _, event := range projection.Tail {
		evidence := recoveryEvent(event)
		switch event.Type {
		case agoprotocol.EventMessageAccepted:
			capsule.Objective.Evidence = []recoveryEvidence{evidence}
			capsule.AcceptanceCriteria.Evidence = []recoveryEvidence{evidence}
			capsule.NextAction.Evidence = []recoveryEvidence{evidence}
		case agoprotocol.EventAssistantCompleted:
			capsule.Decisions.Evidence = []recoveryEvidence{evidence}
		case agoprotocol.EventToolRequested:
			capsule.ChangedPaths.Status = "tool-requests-present-path-effects-unclassified"
			capsule.ChangedPaths.Evidence = appendBoundedEvidence(capsule.ChangedPaths.Evidence, evidence, 4)
			if callID := toolCallID(event); callID != "" {
				requested[callID] = evidence
			}
		case agoprotocol.EventToolCompleted, agoprotocol.EventToolFailed:
			capsule.Verification.Evidence = appendBoundedEvidence(capsule.Verification.Evidence, evidence, 4)
		case agoprotocol.EventToolResultPrepared:
			if callID := preparedToolCallID(event); callID != "" {
				prepared[callID] = true
			}
		}
		capsule.ActiveWork.Evidence = appendBoundedEvidence(capsule.ActiveWork.Evidence, evidence, 12)
	}
	callIDs := make([]string, 0, len(requested))
	for callID := range requested {
		callIDs = append(callIDs, callID)
	}
	sort.Strings(callIDs)
	for _, callID := range callIDs {
		evidence := requested[callID]
		if !prepared[callID] {
			capsule.UnresolvedIssues.Status = "unprepared-tool-requests-present"
			capsule.UnresolvedIssues.Evidence = appendBoundedEvidence(capsule.UnresolvedIssues.Evidence, evidence, 8)
		}
	}
	capsuleJSON, err := json.Marshal(capsule)
	if err != nil {
		return "", err
	}
	if len("AGO_RECOVERY_V2\n")+len(capsuleJSON) > 64<<10 {
		return "", fmt.Errorf("structured recovery capsule exceeds 64 KiB")
	}
	return "AGO_RECOVERY_V2\n" + string(capsuleJSON), nil
}

type recoveryCapsule struct {
	Version            int             `json:"version"`
	SourceSHA256       string          `json:"source_sha256"`
	Objective          recoverySection `json:"objective"`
	AcceptanceCriteria recoverySection `json:"acceptance_criteria"`
	Decisions          recoverySection `json:"decisions"`
	ChangedPaths       recoverySection `json:"changed_paths"`
	Verification       recoverySection `json:"verification"`
	ActiveWork         recoverySection `json:"active_work"`
	UnresolvedIssues   recoverySection `json:"unresolved_issues"`
	NextAction         recoverySection `json:"next_action"`
	PreviousSummary    string          `json:"previous_summary,omitempty"`
}

type recoverySection struct {
	Status   string             `json:"status"`
	Evidence []recoveryEvidence `json:"evidence"`
}

type recoveryEvidence struct {
	Sequence uint64                `json:"sequence"`
	EventID  string                `json:"event_id"`
	Type     agoprotocol.EventType `json:"type"`
	Payload  string                `json:"payload"`
}

func recoveryEvent(event agoprotocol.Event) recoveryEvidence {
	payload := string(event.Payload)
	if len(payload) > 2<<10 {
		payload = payload[:2<<10] + "…"
	}
	return recoveryEvidence{Sequence: event.Sequence, EventID: event.EventID, Type: event.Type, Payload: payload}
}

func appendBoundedEvidence(values []recoveryEvidence, value recoveryEvidence, limit int) []recoveryEvidence {
	values = append(values, value)
	if len(values) > limit {
		values = values[len(values)-limit:]
	}
	return values
}

func toolCallID(event agoprotocol.Event) string {
	var payload struct {
		Event struct {
			CallID string `json:"callId"`
		} `json:"event"`
	}
	_ = json.Unmarshal(event.Payload, &payload)
	return payload.Event.CallID
}

func preparedToolCallID(event agoprotocol.Event) string {
	var payload struct {
		CallID string `json:"call_id"`
	}
	_ = json.Unmarshal(event.Payload, &payload)
	return payload.CallID
}

func (tools productionTools) ExecuteTool(ctx context.Context, call agocoordinator.ToolCall) (agocoordinator.ToolResult, error) {
	if call.Name == productionVerificationToolName {
		return tools.executeVerification(ctx, call)
	}
	plugins, err := tools.manager(ctx, call.ThreadID)
	if err != nil {
		return agocoordinator.ToolResult{}, err
	}
	validate := func(input map[string]any) error {
		if call.Name == "ago_echo" {
			if len(input) != 1 {
				return fmt.Errorf("malformed tool %q", call.Name)
			}
			if _, ok := input["text"].(string); !ok {
				return fmt.Errorf("ago_echo text must be a string")
			}
			return nil
		}
		if call.Name == agowritebroker.ToolNameWriteFile {
			_, _, _, err := declaredWriteFileInput(input)
			return err
		}
		for _, plugin := range plugins.Current().Registrations {
			for _, registered := range plugin.Tools {
				if registered.Name == call.Name {
					return nil
				}
			}
		}
		return fmt.Errorf("unsupported tool %q", call.Name)
	}
	outcome, err := plugins.EvaluateToolCall(ctx, agopluginhost.ToolCallEvent{
		ThreadID: call.ThreadID, TurnID: call.TurnID, ToolCallID: call.CallID, Tool: call.Name, Input: call.Input,
	}, validate)
	if err != nil {
		return agocoordinator.ToolResult{}, err
	}
	switch outcome.Action {
	case agopluginhost.PolicyDeny:
		return agocoordinator.ToolResult{Output: outcome.Message, Error: true}, nil
	case agopluginhost.PolicySynthesize:
		if outcome.Result == nil {
			return agocoordinator.ToolResult{}, fmt.Errorf("policy synthesized no result")
		}
		var output string
		if json.Unmarshal(outcome.Result.Output, &output) != nil {
			output = string(outcome.Result.Output)
		}
		return agocoordinator.ToolResult{Output: output, Error: outcome.Result.Status != "done"}, nil
	case agopluginhost.PolicyAllow:
		if call.Name == "ago_echo" {
			return agocoordinator.ToolResult{Output: "AGO:" + outcome.Input["text"].(string)}, nil
		}
		if call.Name == agowritebroker.ToolNameWriteFile {
			path, content, mode, err := declaredWriteFileInput(outcome.Input)
			if err != nil {
				return agocoordinator.ToolResult{}, err
			}
			result, err := agowritebroker.New(tools.store).WriteFile(ctx, agowritebroker.WriteFileRequest{
				ThreadID: call.ThreadID, Path: path, Content: content, Mode: mode,
				OperationID: call.TurnID, ToolCallID: call.CallID, ToolName: agowritebroker.ToolNameWriteFile,
				IdempotencyKey: "write_file:" + call.CallID,
			})
			if err != nil {
				return agocoordinator.ToolResult{}, err
			}
			encoded, _ := json.Marshal(result)
			return agocoordinator.ToolResult{Output: string(encoded)}, nil
		}
		result, err := plugins.ExecuteToolFor(ctx, call.Name, outcome.Input, agopluginhost.InvocationContext{ThreadID: call.ThreadID, TurnID: call.TurnID})
		if err != nil {
			return agocoordinator.ToolResult{}, err
		}
		var output string
		if json.Unmarshal(result, &output) != nil {
			output = string(result)
		}
		return agocoordinator.ToolResult{Output: output}, nil
	default:
		return agocoordinator.ToolResult{}, fmt.Errorf("unsupported policy outcome %q", outcome.Action)
	}
}

func (tools productionTools) executeVerification(ctx context.Context, call agocoordinator.ToolCall) (agocoordinator.ToolResult, error) {
	if tools.verifier == nil {
		return agocoordinator.ToolResult{}, fmt.Errorf("production verifier is unavailable")
	}
	if len(call.Input) != 1 {
		return agocoordinator.ToolResult{}, fmt.Errorf("%s accepts only check_id", productionVerificationToolName)
	}
	checkID, ok := call.Input["check_id"].(string)
	if !ok || checkID == "" {
		return agocoordinator.ToolResult{}, fmt.Errorf("%s check_id must be a non-empty string", productionVerificationToolName)
	}
	record, runErr := tools.verifier.Run(ctx, agoverifier.Request{
		ThreadID: call.ThreadID, TurnID: call.TurnID, ToolCallID: call.CallID,
		IdempotencyKey: "verify:" + call.CallID, CheckID: checkID,
	})
	if record.RecordID == "" {
		return agocoordinator.ToolResult{}, runErr
	}
	encoded, err := json.Marshal(record)
	if err != nil {
		return agocoordinator.ToolResult{}, fmt.Errorf("encode durable verification result: %w", err)
	}
	return agocoordinator.ToolResult{Output: string(encoded), Error: runErr != nil}, nil
}

func declaredWriteFileInput(input map[string]any) (string, []byte, *uint32, error) {
	if len(input) < 2 || len(input) > 3 {
		return "", nil, nil, fmt.Errorf("write_file accepts path, exactly one content field, and optional mode")
	}
	path, ok := input["path"].(string)
	if !ok || path == "" {
		return "", nil, nil, fmt.Errorf("write_file path must be a non-empty string")
	}
	text, hasText := input["content"].(string)
	encoded, hasEncoded := input["content_base64"].(string)
	if hasText == hasEncoded {
		return "", nil, nil, fmt.Errorf("write_file requires exactly one of content or content_base64")
	}
	content := []byte(text)
	if hasEncoded {
		var err error
		content, err = base64.StdEncoding.Strict().DecodeString(encoded)
		if err != nil {
			return "", nil, nil, fmt.Errorf("write_file content_base64: %w", err)
		}
	}
	var mode *uint32
	if value, present := input["mode"]; present {
		number, ok := value.(float64)
		if !ok || number < 1 || number > 0o777 || number != float64(uint32(number)) {
			return "", nil, nil, fmt.Errorf("write_file mode must be an integer from 1 through 511")
		}
		parsed := uint32(number)
		mode = &parsed
	}
	for key := range input {
		if key != "path" && key != "content" && key != "content_base64" && key != "mode" {
			return "", nil, nil, fmt.Errorf("write_file contains unknown field %q", key)
		}
	}
	return path, content, mode, nil
}

func (tools productionTools) ObserveToolResult(ctx context.Context, call agocoordinator.ToolCall, confirmed agocoordinator.ToolResult) agocoordinator.ToolResult {
	plugins, err := tools.manager(ctx, call.ThreadID)
	if err != nil {
		return confirmed
	}
	status := "done"
	if confirmed.Error {
		status = "error"
	}
	encoded, _ := json.Marshal(confirmed.Output)
	observed := plugins.ObserveToolResult(ctx, call, agopluginhost.ToolResult{Status: status, Output: encoded, Error: map[bool]string{true: confirmed.Output}[confirmed.Error]}, nil)
	var output string
	if json.Unmarshal(observed.Output, &output) != nil {
		output = string(observed.Output)
	}
	if observed.Error != "" {
		output = observed.Error
	}
	return agocoordinator.ToolResult{Output: output, Error: observed.Status != "done"}
}

func (tools productionTools) ObserveLifecycle(ctx context.Context, hook string, payload any) {
	encoded, _ := json.Marshal(payload)
	var correlation struct {
		ThreadID string `json:"thread_id"`
	}
	if json.Unmarshal(encoded, &correlation) != nil || correlation.ThreadID == "" {
		return
	}
	plugins, err := tools.manager(ctx, correlation.ThreadID)
	if err == nil {
		plugins.ObserveLifecycle(ctx, hook, payload, nil)
	}
}

func (tools productionTools) manager(ctx context.Context, threadID string) (*agopluginhost.Manager, error) {
	thread, err := tools.store.Thread(ctx, threadID)
	if err != nil {
		return nil, err
	}
	return tools.plugins.Get(ctx, thread.Workspace)
}

func (tools productionTools) PluginRegistrations(ctx context.Context, threadID string) (agopluginhost.Snapshot, error) {
	manager, err := tools.manager(ctx, threadID)
	if err != nil {
		return agopluginhost.Snapshot{}, err
	}
	return manager.Current(), nil
}

func (tools productionTools) ExternalTools(ctx context.Context, threadID string) ([]agocoordinator.ExternalTool, error) {
	manager, err := tools.manager(ctx, threadID)
	if err != nil {
		return nil, err
	}
	result := []agocoordinator.ExternalTool{
		{Name: "ago_echo", Description: "Call Ago's external echo tool", InputSchema: json.RawMessage(`{"type":"object","properties":{"text":{"type":"string"}},"required":["text"],"additionalProperties":false}`)},
		{Name: agowritebroker.ToolNameWriteFile, Description: "Write exact bytes to one declared repository-relative path and return a durable receipt", InputSchema: json.RawMessage(`{"type":"object","properties":{"path":{"type":"string"},"content":{"type":"string"},"content_base64":{"type":"string"},"mode":{"type":"integer","minimum":1,"maximum":511}},"required":["path"],"oneOf":[{"required":["content"]},{"required":["content_base64"]}],"additionalProperties":false}`)},
	}
	if tools.verifier != nil {
		result = append(result, productionVerificationTool())
	}
	for _, plugin := range manager.Current().Registrations {
		for _, tool := range plugin.Tools {
			if tool.Name == "ago_echo" || tool.Name == agowritebroker.ToolNameWriteFile || tool.Name == productionVerificationToolName {
				return nil, fmt.Errorf("plugin %q registered reserved tool %q", plugin.PluginID, tool.Name)
			}
			result = append(result, agocoordinator.ExternalTool{Name: tool.Name, Description: tool.Description, InputSchema: tool.InputSchema})
		}
	}
	return result, nil
}

func (tools productionTools) ExecutePluginCommand(ctx context.Context, threadID, turnID, commandID string, input any) (json.RawMessage, error) {
	mailbox, err := tools.store.Mailbox(ctx, threadID)
	if err != nil {
		return nil, err
	}
	if turnID == "" || mailbox.ActiveTurnID != turnID {
		return nil, fmt.Errorf("plugin command turn %q is not the active turn %q", turnID, mailbox.ActiveTurnID)
	}
	manager, err := tools.manager(ctx, threadID)
	if err != nil {
		return nil, err
	}
	return manager.ExecuteCommandFor(ctx, commandID, input, agopluginhost.InvocationContext{ThreadID: threadID, TurnID: turnID})
}

// The initial route is Ago-owned. It is replaced by the capability router in
// Phase 6; users never choose or configure a provider/model.
func initialInferenceRoute() (provider, model string) {
	return "openai", "gpt-5.4"
}

func envOrDefault(key, fallback string) string {
	if value := strings.TrimSpace(os.Getenv(key)); value != "" {
		return value
	}
	return fallback
}

func resolveExecutable(command string) (string, error) {
	path, err := exec.LookPath(strings.TrimSpace(command))
	if err != nil {
		return "", err
	}
	path, err = filepath.Abs(path)
	if err != nil {
		return "", err
	}
	path, err = filepath.EvalSymlinks(path)
	if err != nil {
		return "", err
	}
	info, err := os.Stat(path)
	if err != nil {
		return "", err
	}
	if info.IsDir() || info.Mode()&0o111 == 0 {
		return "", fmt.Errorf("%s is not executable", path)
	}
	return path, nil
}

type tcpStartupFlags struct {
	Listen, EndpointFile, BearerToken string
}

type tcpStartupConfig struct {
	listen, endpointFile, bearerToken string
}

func loadOptionalTCPStartupConfig(flags tcpStartupFlags) (*tcpStartupConfig, error) {
	listen := strings.TrimSpace(flags.Listen)
	endpoint := strings.TrimSpace(flags.EndpointFile)
	configured := 0
	for _, value := range []string{listen, endpoint, flags.BearerToken} {
		if value != "" {
			configured++
		}
	}
	if configured == 0 {
		return nil, nil
	}
	if configured != 3 {
		return nil, fmt.Errorf("--tcp-listen, --tcp-endpoint-file, and --tcp-bearer-token must be configured together")
	}
	if err := validateLoopbackAddress(listen); err != nil {
		return nil, err
	}
	if err := validatePrivateEndpointPath(endpoint); err != nil {
		return nil, err
	}
	if _, err := agodaemon.RequireBearerToken(http.NotFoundHandler(), flags.BearerToken); err != nil {
		return nil, err
	}
	return &tcpStartupConfig{listen: listen, endpointFile: endpoint, bearerToken: flags.BearerToken}, nil
}

func validateLoopbackAddress(address string) error {
	host, port, err := net.SplitHostPort(address)
	if err != nil {
		return fmt.Errorf("invalid TCP listen address: %w", err)
	}
	ip := net.ParseIP(host)
	if ip == nil || !ip.IsLoopback() {
		return fmt.Errorf("TCP listener must use a numeric loopback address")
	}
	parsedPort, err := strconv.ParseUint(port, 10, 16)
	if err != nil || parsedPort > 65535 {
		return fmt.Errorf("TCP listener port must be between 0 and 65535")
	}
	return nil
}

func validatePrivateEndpointPath(path string) error {
	if path == "" || !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return fmt.Errorf("TCP endpoint file must be a canonical absolute path")
	}
	parent := filepath.Dir(path)
	canonicalParent, err := filepath.EvalSymlinks(parent)
	if err != nil || canonicalParent != parent {
		return fmt.Errorf("TCP endpoint parent must be an existing canonical directory")
	}
	info, err := os.Stat(parent)
	if err != nil || !info.IsDir() || info.Mode().Perm()&0o077 != 0 {
		return fmt.Errorf("TCP endpoint parent must be a private directory")
	}
	if info, err := os.Lstat(path); err == nil {
		if !info.Mode().IsRegular() || info.Mode().Perm() != 0o600 {
			return fmt.Errorf("existing TCP endpoint must be a private regular file")
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("inspect TCP endpoint file: %w", err)
	}
	return nil
}

func tcpListener(address string) (net.Listener, error) {
	if err := validateLoopbackAddress(address); err != nil {
		return nil, err
	}
	listener, err := net.Listen("tcp", address)
	if err != nil {
		return nil, fmt.Errorf("listen on loopback TCP: %w", err)
	}
	tcpAddress, ok := listener.Addr().(*net.TCPAddr)
	if !ok || tcpAddress.IP == nil || !tcpAddress.IP.IsLoopback() {
		_ = listener.Close()
		return nil, fmt.Errorf("resolved TCP listener is not loopback-only")
	}
	return listener, nil
}

func writeTCPEndpoint(path string, address net.Addr) (string, error) {
	if err := validatePrivateEndpointPath(path); err != nil {
		return "", err
	}
	tcpAddress, ok := address.(*net.TCPAddr)
	if !ok || tcpAddress.IP == nil || !tcpAddress.IP.IsLoopback() || tcpAddress.Port <= 0 {
		return "", fmt.Errorf("TCP endpoint address must be an allocated loopback address")
	}
	baseURL := "http://" + net.JoinHostPort(tcpAddress.IP.String(), strconv.Itoa(tcpAddress.Port))
	encoded, err := json.Marshal(struct {
		BaseURL string `json:"base_url"`
	}{BaseURL: baseURL})
	if err != nil {
		return "", err
	}
	encoded = append(encoded, '\n')
	var random [12]byte
	if _, err := rand.Read(random[:]); err != nil {
		return "", fmt.Errorf("generate TCP endpoint temporary name: %w", err)
	}
	temp := filepath.Join(filepath.Dir(path), ".ago-endpoint-"+base64.RawURLEncoding.EncodeToString(random[:]))
	file, err := os.OpenFile(temp, os.O_WRONLY|os.O_CREATE|os.O_EXCL|syscall.O_NOFOLLOW, 0o600)
	if err != nil {
		return "", fmt.Errorf("create TCP endpoint temporary file: %w", err)
	}
	removeTemp := true
	defer func() {
		_ = file.Close()
		if removeTemp {
			_ = os.Remove(temp)
		}
	}()
	if _, err := file.Write(encoded); err != nil {
		return "", fmt.Errorf("write TCP endpoint: %w", err)
	}
	if err := file.Sync(); err != nil {
		return "", fmt.Errorf("fsync TCP endpoint: %w", err)
	}
	if err := file.Close(); err != nil {
		return "", fmt.Errorf("close TCP endpoint: %w", err)
	}
	if err := validatePrivateEndpointPath(path); err != nil {
		return "", err
	}
	if err := os.Rename(temp, path); err != nil {
		return "", fmt.Errorf("publish TCP endpoint: %w", err)
	}
	removeTemp = false
	if err := syncDirectory(filepath.Dir(path)); err != nil {
		_ = os.Remove(path)
		_ = syncDirectory(filepath.Dir(path))
		return "", fmt.Errorf("fsync TCP endpoint directory: %w", err)
	}
	return baseURL, nil
}

func removeTCPEndpoint(path string) error {
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() || info.Mode().Perm() != 0o600 {
		return fmt.Errorf("refusing to remove non-private TCP endpoint file")
	}
	if err := os.Remove(path); err != nil {
		return err
	}
	return syncDirectory(filepath.Dir(path))
}

func syncDirectory(path string) error {
	directory, err := os.Open(path)
	if err != nil {
		return err
	}
	defer directory.Close()
	return directory.Sync()
}

func unixListener(path string) (net.Listener, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("create socket directory: %w", err)
	}
	if info, err := os.Lstat(path); err == nil {
		if info.Mode()&os.ModeSocket == 0 {
			return nil, fmt.Errorf("refusing to replace non-socket path %s", path)
		}
		connection, dialErr := net.DialTimeout("unix", path, 200*time.Millisecond)
		if dialErr == nil {
			_ = connection.Close()
			return nil, fmt.Errorf("Ago daemon is already listening on %s", path)
		}
		if err := os.Remove(path); err != nil {
			return nil, fmt.Errorf("remove stale socket: %w", err)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	listener, err := net.Listen("unix", path)
	if err != nil {
		return nil, fmt.Errorf("listen on Unix socket: %w", err)
	}
	if err := os.Chmod(path, 0o600); err != nil {
		_ = listener.Close()
		return nil, fmt.Errorf("secure Unix socket: %w", err)
	}
	return listener, nil
}
