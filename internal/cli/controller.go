package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

func (a App) controllerServe(ctx context.Context, args []string) error {
	if err := controllerHostSupported(); err != nil {
		return exit(2, "%v", err)
	}
	stateFile, err := controllerDefaultStateFile()
	if err != nil {
		return err
	}
	defaultBinary, err := os.Executable()
	if err != nil {
		return exit(2, "resolve crabbox executable: %v", err)
	}
	fs := newFlagSet("adapter serve", a.Stderr)
	listen := fs.String("listen", getenv("CRABBOX_ADAPTER_LISTEN", "127.0.0.1:8787"), "HTTP listen address")
	unixSocket := fs.String("unix-socket", getenv("CRABBOX_ADAPTER_UNIX_SOCKET", ""), "optional current-user-owned Unix socket served alongside HTTP")
	tokenFile := fs.String("token-file", getenv("CRABBOX_ADAPTER_TOKEN_FILE", ""), "file containing the bearer token (required)")
	statePath := fs.String("state-file", getenv("CRABBOX_ADAPTER_STATE_FILE", stateFile), "durable adapter JSON state file")
	configPath := fs.String("config", getenv("CRABBOX_ADAPTER_CONFIG", ""), "Crabbox config file used by child commands")
	provider := fs.String("provider", getenv("CRABBOX_ADAPTER_PROVIDER", ""), "fixed provider used for every workspace")
	profile := fs.String("profile", getenv("CRABBOX_ADAPTER_PROFILE", ""), "fixed accepted workspace profile")
	adapterID := fs.String("id", getenv("CRABBOX_ADAPTER_ID", ""), "coordinator adapter ID recorded with registered workspaces")
	maxConcurrent := fs.Int("max-concurrent", getenvInt("CRABBOX_ADAPTER_MAX_CONCURRENT", 2), "maximum concurrent lifecycle operations")
	allowDesktop := fs.Bool("allow-desktop", controllerEnvBool("CRABBOX_ADAPTER_ALLOW_DESKTOP"), "allow desktop-capable workspace requests")
	allowBrowser := fs.Bool("allow-browser", controllerEnvBool("CRABBOX_ADAPTER_ALLOW_BROWSER"), "allow browser-capable workspace requests")
	allowCode := fs.Bool("allow-code", controllerEnvBool("CRABBOX_ADAPTER_ALLOW_CODE"), "allow code-server-capable workspace requests")
	attachURLTemplate := fs.String("attach-url-template", getenv("CRABBOX_ADAPTER_ATTACH_URL_TEMPLATE", ""), "optional WSS terminal connection URL template")
	vncURLTemplate := fs.String("vnc-url-template", getenv("CRABBOX_ADAPTER_VNC_URL_TEMPLATE", ""), "published desktop URL after bridge verification")
	createTimeout := fs.Duration("create-timeout", controllerEnvDuration("CRABBOX_ADAPTER_CREATE_TIMEOUT", 60*time.Minute), "maximum workspace creation duration")
	inspectTimeout := fs.Duration("inspect-timeout", controllerEnvDuration("CRABBOX_ADAPTER_INSPECT_TIMEOUT", 2*time.Minute), "maximum workspace inspection duration")
	stopTimeout := fs.Duration("stop-timeout", controllerEnvDuration("CRABBOX_ADAPTER_STOP_TIMEOUT", 10*time.Minute), "maximum workspace stop duration")
	connectionTimeout := fs.Duration("connection-timeout", controllerEnvDuration("CRABBOX_ADAPTER_CONNECTION_TIMEOUT", 2*time.Minute), "maximum desktop connection setup duration")
	readyReconcileInterval := fs.Duration("ready-reconcile-interval", controllerEnvDuration("CRABBOX_ADAPTER_READY_RECONCILE_INTERVAL", time.Minute), "maximum interval between ready workspace inspections")
	requiredTTL := fs.Duration("required-ttl", 0, "required ttlSeconds value for every workspace request")
	requiredIdleTimeout := fs.Duration("required-idle-timeout", 0, "required idleTimeoutSeconds value for every workspace request")
	forbidClassOverride := fs.Bool("forbid-class-override", controllerEnvBool("CRABBOX_ADAPTER_FORBID_CLASS_OVERRIDE"), "reject nonempty request class values")
	forbidServerTypeOverride := fs.Bool("forbid-server-type-override", controllerEnvBool("CRABBOX_ADAPTER_FORBID_SERVER_TYPE_OVERRIDE"), "reject nonempty request serverType values")
	binary := fs.String("crabbox-binary", getenv("CRABBOX_ADAPTER_BINARY", defaultBinary), "Crabbox executable used for lifecycle commands")
	workDir := fs.String("work-dir", getenv("CRABBOX_ADAPTER_WORK_DIR", ""), "working directory for lifecycle commands")
	if err := parseFlags(fs, args); err != nil {
		return err
	}
	if err := controllerPolicyEnvDefault(fs, "required-ttl", "CRABBOX_ADAPTER_REQUIRED_TTL", requiredTTL); err != nil {
		return exit(2, "%v", err)
	}
	if err := controllerPolicyEnvDefault(fs, "required-idle-timeout", "CRABBOX_ADAPTER_REQUIRED_IDLE_TIMEOUT", requiredIdleTimeout); err != nil {
		return exit(2, "%v", err)
	}
	if fs.NArg() != 0 {
		return exit(2, "usage: crabbox adapter serve [flags]")
	}
	if strings.TrimSpace(*tokenFile) == "" {
		return exit(2, "--token-file is required")
	}
	if *maxConcurrent < 1 || *maxConcurrent > 64 {
		return exit(2, "--max-concurrent must be between 1 and 64")
	}
	for name, value := range map[string]time.Duration{
		"create-timeout":           *createTimeout,
		"inspect-timeout":          *inspectTimeout,
		"stop-timeout":             *stopTimeout,
		"connection-timeout":       *connectionTimeout,
		"ready-reconcile-interval": *readyReconcileInterval,
	} {
		if value <= 0 {
			return exit(2, "--%s must be greater than zero", name)
		}
	}
	if strings.TrimSpace(*vncURLTemplate) != "" && !*allowDesktop {
		return exit(2, "--vnc-url-template requires --allow-desktop")
	}
	requiredTTLSeconds, requiredIdleSeconds, err := controllerPolicyLeaseValues(*requiredTTL, *requiredIdleTimeout)
	if err != nil {
		return exit(2, "%v", err)
	}
	if strings.TrimSpace(*provider) != "" {
		if _, err := ProviderFor(*provider); err != nil {
			return err
		}
	}
	if strings.TrimSpace(*adapterID) != "" && !validControllerWorkspaceID(strings.TrimSpace(*adapterID)) {
		return exit(2, "--id must be a lowercase DNS-style name of at most 63 characters")
	}
	token, err := readAdapterToken(*tokenFile)
	if err != nil {
		return err
	}
	opts := controllerServiceOptions{
		StateFile:                expandUserPath(strings.TrimSpace(*statePath)),
		MaxConcurrent:            *maxConcurrent,
		Allowed:                  controllerCapabilities{Desktop: *allowDesktop, Browser: *allowBrowser, Code: *allowCode},
		Profile:                  strings.TrimSpace(*profile),
		AttachURLTemplate:        strings.TrimSpace(*attachURLTemplate),
		VNCURLTemplate:           strings.TrimSpace(*vncURLTemplate),
		CreateTimeout:            *createTimeout,
		InspectTimeout:           *inspectTimeout,
		StopTimeout:              *stopTimeout,
		ConnectionTimeout:        *connectionTimeout,
		ReadyReconcileInterval:   *readyReconcileInterval,
		RequiredTTLSeconds:       requiredTTLSeconds,
		RequiredIdleSeconds:      requiredIdleSeconds,
		ForbidClassOverride:      *forbidClassOverride,
		ForbidServerTypeOverride: *forbidServerTypeOverride,
	}
	runnerConfig := expandUserPath(strings.TrimSpace(*configPath))
	runnerProvider := strings.TrimSpace(*provider)
	runnerWorkDir := expandUserPath(strings.TrimSpace(*workDir))
	credentialBoundary, err := controllerRunnerCredentialBoundary(runnerConfig, runnerProvider, runnerWorkDir)
	if err != nil {
		return exit(2, "resolve controller child credential boundary: %v", err)
	}
	runner := &execControllerWorkspaceRunner{opts: execControllerRunnerOptions{
		Binary:                      expandUserPath(strings.TrimSpace(*binary)),
		Config:                      runnerConfig,
		Provider:                    runnerProvider,
		TargetOS:                    credentialBoundary.TargetOS,
		ExternalDesktopPasswordEnv:  credentialBoundary.CurrentDesktopPasswordEnv,
		ExternalDesktopPasswordEnvs: credentialBoundary.DesktopPasswordEnvs,
		ResolveCredentialBoundary:   true,
		WorkDir:                     runnerWorkDir,
		StateFile:                   opts.StateFile,
		AdapterID:                   strings.TrimSpace(*adapterID),
	}}
	serviceCtx, cancelService := context.WithCancel(ctx)
	defer cancelService()
	service, err := newControllerService(serviceCtx, opts, runner, token, a.Stderr)
	if err != nil {
		return exit(2, "%v", err)
	}
	listener, err := net.Listen("tcp", strings.TrimSpace(*listen))
	if err != nil {
		cancelService()
		service.waitForShutdown()
		return exit(5, "listen on %s: %v", *listen, err)
	}
	defer listener.Close()
	listeners := []net.Listener{listener}
	listenerNames := []string{"tcp=" + listener.Addr().String()}
	if socketPath := strings.TrimSpace(*unixSocket); socketPath != "" {
		unixListener, cleanupSocket, listenErr := listenAdapterUnixSocket(socketPath)
		if listenErr != nil {
			cancelService()
			service.waitForShutdown()
			return exit(5, "--unix-socket: %v", listenErr)
		}
		defer cleanupSocket()
		defer unixListener.Close()
		listeners = append(listeners, unixListener)
		listenerNames = append(listenerNames, "unix="+socketPath)
	}
	server := &http.Server{
		Handler:           service,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      *connectionTimeout + 10*time.Second,
		IdleTimeout:       90 * time.Second,
		MaxHeaderBytes:    32 << 10,
		BaseContext: func(net.Listener) context.Context {
			return serviceCtx
		},
	}
	service.startReconciliation()
	fmt.Fprintf(a.Stderr, "adapter listening=%s state=%s provider=%s profile=%s id=%s max_concurrent=%d\n", strings.Join(listenerNames, ","), opts.StateFile, blank(*provider, "config"), blank(*profile, "default"), blank(*adapterID, "unbound"), opts.MaxConcurrent)
	shutdownDone := make(chan struct{})
	go func() {
		defer close(shutdownDone)
		<-serviceCtx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = server.Shutdown(shutdownCtx)
	}()
	serveErrors := make(chan error, len(listeners))
	for _, current := range listeners {
		go func(current net.Listener) {
			serveErrors <- server.Serve(current)
		}(current)
	}
	err = <-serveErrors
	cancelService()
	<-shutdownDone
	for range listeners[1:] {
		<-serveErrors
	}
	service.waitForShutdown()
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}

func (a App) controllerStateValidate(args []string) error {
	fs := newFlagSet("adapter state validate", a.Stderr)
	statePath := fs.String("state-file", "", "adapter JSON state file to validate")
	if err := parseFlags(fs, args); err != nil {
		return err
	}
	if fs.NArg() != 0 || strings.TrimSpace(*statePath) == "" {
		return exit(2, "usage: crabbox adapter state validate --state-file <path>")
	}
	path := expandUserPath(strings.TrimSpace(*statePath))
	if _, err := os.Lstat(path); err != nil {
		return exit(2, "stat adapter state: %v", err)
	}
	state, err := loadControllerState(path)
	if err != nil {
		return exit(2, "validate adapter state: %v", err)
	}
	fmt.Fprintf(a.Stdout, "adapter state valid version=%d workspaces=%d\n", state.Version, len(state.Workspaces))
	return nil
}

func controllerPolicyLeaseSeconds(name string, value time.Duration) (int, error) {
	if value == 0 {
		return 0, nil
	}
	if value%time.Second != 0 {
		return 0, fmt.Errorf("--%s must be a whole number of seconds", name)
	}
	seconds64 := int64(value / time.Second)
	if seconds64 < 60 || seconds64 > 7*24*60*60 {
		return 0, fmt.Errorf("--%s must be between 1m and 168h", name)
	}
	seconds := int(seconds64)
	if err := validateControllerLeaseSeconds(name, seconds); err != nil {
		return 0, fmt.Errorf("--%s must be between 1m and 168h", name)
	}
	return seconds, nil
}

func controllerPolicyLeaseValues(ttl, idle time.Duration) (int, int, error) {
	ttlSeconds, err := controllerPolicyLeaseSeconds("required-ttl", ttl)
	if err != nil {
		return 0, 0, err
	}
	idleSeconds, err := controllerPolicyLeaseSeconds("required-idle-timeout", idle)
	if err != nil {
		return 0, 0, err
	}
	if ttlSeconds > 0 && idleSeconds > ttlSeconds {
		return 0, 0, fmt.Errorf("--required-idle-timeout must not exceed --required-ttl")
	}
	return ttlSeconds, idleSeconds, nil
}

func controllerPolicyEnvDuration(name string) (time.Duration, error) {
	value := strings.TrimSpace(os.Getenv(name))
	if value == "" {
		return 0, nil
	}
	parsed, err := time.ParseDuration(value)
	if err != nil {
		return 0, fmt.Errorf("%s must be a duration: %w", name, err)
	}
	return parsed, nil
}

func controllerPolicyEnvDefault(fs *flag.FlagSet, flagName, envName string, value *time.Duration) error {
	explicit := false
	fs.Visit(func(current *flag.Flag) {
		if current.Name == flagName {
			explicit = true
		}
	})
	if explicit {
		return nil
	}
	parsed, err := controllerPolicyEnvDuration(envName)
	if err != nil {
		return err
	}
	*value = parsed
	return nil
}

func controllerDefaultStateFile() (string, error) {
	dir, err := crabboxStateDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "adapter", "state.json"), nil
}

func readAdapterToken(path string) (string, error) {
	return readPrivateAdapterToken(path, "adapter")
}

func readPrivateAdapterToken(path, label string) (string, error) {
	path = expandUserPath(strings.TrimSpace(path))
	file, err := openControllerTokenFile(path)
	if err != nil {
		return "", exit(2, "read %s token file: %v", label, err)
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return "", exit(2, "stat %s token file: %v", label, err)
	}
	if !info.Mode().IsRegular() {
		return "", exit(2, "%s token file must be a regular file", label)
	}
	if info.Mode().Perm()&0o077 != 0 {
		return "", exit(2, "%s token file %s must not be accessible by group or others", label, path)
	}
	if info.Size() > 8<<10 {
		return "", exit(2, "%s token file is too large", label)
	}
	data, err := io.ReadAll(io.LimitReader(file, (8<<10)+1))
	if err != nil {
		return "", exit(2, "read %s token file: %v", label, err)
	}
	if len(data) > 8<<10 {
		return "", exit(2, "%s token file is too large", label)
	}
	token := strings.TrimSpace(string(data))
	if token == "" {
		return "", exit(2, "%s token file is empty", label)
	}
	if strings.ContainsAny(token, "\r\n") {
		return "", exit(2, "%s token file must contain one token", label)
	}
	return token, nil
}

func controllerEnvBool(name string) bool {
	value, ok := getenvBool(name)
	return ok && value
}

func controllerEnvDuration(name string, fallback time.Duration) time.Duration {
	value := strings.TrimSpace(os.Getenv(name))
	if value == "" {
		return fallback
	}
	parsed, err := time.ParseDuration(value)
	if err != nil {
		return fallback
	}
	return parsed
}
