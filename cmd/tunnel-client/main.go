package main

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/tls"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	mrand "math/rand"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
	"unicode/utf8"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/gorilla/websocket"
	"github.com/skip2/go-qrcode"
	"github.com/zero469/opencode-relay-server/internal/tunnel"
	"golang.org/x/crypto/scrypt"
	"golang.org/x/term"
)

var (
	buildVersion = "dev"
	buildTime    = "unknown"
	buildCommit  = "unknown"
)

const (
	configDir        = ".opencode-tunnel"
	authFileName     = "auth.json"
	deviceFileName   = "device.json"
	opencodeFileName = "opencode.json"
	defaultRelay     = "https://opencode-relay.azurewebsites.net"
	defaultPort      = "4096"
	pairingInterval  = 2 * time.Second

	keyKdfScrypt      = "scrypt-n15-r8-p1"
	keyReportAttempts = 3

	// tunnelProtocolVersion is the streaming protocol the client announces in
	// the upgrade request. Relay answers with X-Tunnel-Protocol on the
	// handshake response; anything >= this enables streaming.
	tunnelProtocolVersion = 2
	// streamChunkSize is the maximum body slice carried by one res-chunk.
	// Encrypting adds 28 bytes (12-byte nonce + 16-byte tag), which stays well
	// inside MaxFrameBytes.
	streamChunkSize = 32 * 1024
	// streamE2EHeader marks whether streaming payloads are E2E encrypted:
	// "1" when EncryptionKey is set, "0" for plaintext.
	streamE2EHeader = "X-E2E-Stream"
)

// keyReportRetryDelay is a var so tests can skip the backoff waits.
var keyReportRetryDelay = time.Second

type AuthConfig struct {
	RelayURL string `json:"relay_url"`
	Token    string `json:"token"`
	Email    string `json:"email"`
}

type DeviceConfig struct {
	DeviceID     int64  `json:"id"`
	DeviceName   string `json:"name"`
	RelayURL     string `json:"relay_url"`
	Subdomain    string `json:"subdomain"`
	AuthUser     string `json:"auth_user"`
	AuthPassword string `json:"auth_password"`
	// EncryptionKey is the hex E2E host key. It is stored locally in plaintext
	// and protected only by device.json's 0600 permissions; the passphrase-wrapped
	// copy (KeyWrapped) protects only the relay-side copy.
	EncryptionKey string `json:"encryption_key,omitempty"`
	HostID        string `json:"host_id,omitempty"`
	HostName      string `json:"host_name,omitempty"`
	KeyWrapped    string `json:"key_wrapped,omitempty"`
	KeySalt       string `json:"key_salt,omitempty"`
	KeyKdf        string `json:"key_kdf,omitempty"`
}

type OpenCodeConfig struct {
	Command   string `json:"command"`
	WorkDir   string `json:"workdir"`
	Port      string `json:"port,omitempty"`
	AutoStart bool   `json:"auto_start"`
	// AuthUser/AuthPassword are the local OpenCode server's HTTP Basic
	// credentials. They are applied by the tunnel when forwarding, so the relay
	// never sees them and clients never have to send them.
	AuthUser     string `json:"auth_user,omitempty"`
	AuthPassword string `json:"auth_password,omitempty"`
}

type LoginResponse struct {
	Token string `json:"token"`
	User  struct {
		ID    int64  `json:"id"`
		Email string `json:"email"`
	} `json:"user"`
}

type PairingResponse struct {
	ID          string    `json:"id"`
	PairingCode string    `json:"pairing_code"`
	ExpiresAt   time.Time `json:"expires_at"`
}

type PairingStatusResponse struct {
	Status string        `json:"status"`
	Device *DeviceConfig `json:"device,omitempty"`
}

type QRData struct {
	Version       int    `json:"v"`
	RelayURL      string `json:"r"`
	PairingID     string `json:"p"`
	PairingCode   string `json:"c"`
	Hostname      string `json:"h,omitempty"`
	EncryptionKey string `json:"k,omitempty"`
}

type TunnelRequest struct {
	ID      string            `json:"id"`
	Method  string            `json:"method"`
	Path    string            `json:"path"`
	Headers map[string]string `json:"headers"`
	Body    []byte            `json:"body"`
}

type TunnelResponse struct {
	ID         string            `json:"id"`
	StatusCode int               `json:"status_code"`
	Headers    map[string]string `json:"headers"`
	Body       []byte            `json:"body"`
}

type HeartbeatMessage struct {
	Type string `json:"type"`
	Ts   int64  `json:"ts"`
}

type SSEEvent struct {
	Type       string          `json:"type"`
	Properties json.RawMessage `json:"properties"`
}

type TunnelEvent struct {
	Event string `json:"event"`
	Data  string `json:"data"`
}

type TunnelClient struct {
	config       *DeviceConfig
	localPort    string
	conn         *websocket.Conn
	writeMu      sync.Mutex
	httpClient   *http.Client
	sseStopChan  chan struct{}
	sseWaitGroup sync.WaitGroup
	// sseStopMu guards the one-shot close of sseStopChan so connect()'s own
	// teardown and an external Stop() cannot close it twice.
	sseStopMu  sync.Mutex
	sseStopped bool

	// streamMode is re-negotiated on every connect(). When false the client
	// uses the legacy single-frame response path. modeMu keeps handlers of a
	// dying connection from racing with the next negotiation.
	modeMu     sync.RWMutex
	streamMode bool

	// streamCancels maps an in-flight request ID to its cancel handle so
	// req-cancel and connection teardown can abort the local request.
	streamsMu     sync.Mutex
	streamCancels map[string]*streamCancel

	// localAuth* apply the local OpenCode server's Basic credentials to
	// forwarded HTTP and WebSocket requests. Empty means no local auth.
	localAuthUser     string
	localAuthPassword string

	// wsConns holds local WebSocket sessions by tunnel request ID.
	wsMu    sync.Mutex
	wsConns map[string]*wsSession
}

// streamCancel is an identity token so a replaced stream cannot unregister the
// cancel handle of a newer stream that reused the same ID.
type streamCancel struct {
	cancel context.CancelFunc
}

// wsSession is one local WebSocket bridged to the relay. conn is nil while the
// dial is still in flight; writeMu serialises writes to the local socket.
// encrypted mirrors WsAck.Encrypted for this session so both directions agree
// even if the device config changes later.
type wsSession struct {
	conn      *websocket.Conn
	writeMu   sync.Mutex
	once      sync.Once
	encrypted bool
}

// sseReconnectDelay is how long subscribeSSE waits before resubscribing after a
// failed or dropped stream. It is a variable so tests can shorten it.
var sseReconnectDelay = 3 * time.Second

// imageCache stores base64 image URLs by partID for lazy loading
var imageCache sync.Map

// dnsCache stores resolved IPs from DNS-over-HTTPS for the process lifetime
var dnsCache sync.Map

// DoH response structures for Cloudflare DNS-over-HTTPS JSON API
type dohResponse struct {
	Status int         `json:"Status"`
	Answer []dohAnswer `json:"Answer"`
}

type dohAnswer struct {
	Name string `json:"name"`
	Type int    `json:"type"`
	TTL  int    `json:"TTL"`
	Data string `json:"data"`
}

// debugLogger writes debug/info logs to file only
var debugLogger *log.Logger
var debugLogFile *os.File

func initDebugLogger() {
	dir := getConfigDir()
	if err := os.MkdirAll(dir, 0700); err != nil {
		return
	}
	logPath := filepath.Join(dir, "tunnel.log")

	if info, err := os.Stat(logPath); err == nil && info.Size() > 10*1024*1024 {
		os.Remove(logPath)
	}

	file, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
	if err != nil {
		return
	}
	debugLogFile = file
	debugLogger = log.New(file, "", log.LstdFlags)
}

func debugLog(format string, v ...interface{}) {
	if debugLogger != nil {
		debugLogger.Printf(format, v...)
	}
}

func resolveViaDoH(hostname string) (string, error) {
	if cached, ok := dnsCache.Load(hostname); ok {
		return cached.(string), nil
	}

	dohTransport := &http.Transport{
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			return (&net.Dialer{Timeout: 5 * time.Second}).DialContext(ctx, network, "1.1.1.1:443")
		},
	}
	dohClient := &http.Client{Timeout: 5 * time.Second, Transport: dohTransport}

	dohURL := fmt.Sprintf("https://1.1.1.1/dns-query?name=%s&type=A", url.QueryEscape(hostname))
	req, err := http.NewRequest("GET", dohURL, nil)
	if err != nil {
		debugLog("[DNS] DoH request creation failed for %s: %v, falling back to system DNS", hostname, err)
		return resolveViaSystemDNS(hostname)
	}
	req.Header.Set("Accept", "application/dns-json")

	resp, err := dohClient.Do(req)
	if err != nil {
		debugLog("[DNS] DoH request failed for %s: %v, falling back to system DNS", hostname, err)
		return resolveViaSystemDNS(hostname)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		debugLog("[DNS] DoH returned status %d for %s, falling back to system DNS", resp.StatusCode, hostname)
		return resolveViaSystemDNS(hostname)
	}

	var dohResp dohResponse
	if err := json.NewDecoder(resp.Body).Decode(&dohResp); err != nil {
		debugLog("[DNS] DoH response parse failed for %s: %v, falling back to system DNS", hostname, err)
		return resolveViaSystemDNS(hostname)
	}

	for _, answer := range dohResp.Answer {
		if answer.Type == 1 {
			dnsCache.Store(hostname, answer.Data)
			debugLog("[DNS] Resolved %s -> %s via DoH", hostname, answer.Data)
			return answer.Data, nil
		}
	}

	debugLog("[DNS] No A record found via DoH for %s, falling back to system DNS", hostname)
	return resolveViaSystemDNS(hostname)
}

func resolveViaSystemDNS(hostname string) (string, error) {
	debugLog("[DNS] Falling back to system DNS for %s", hostname)
	addrs, err := net.DefaultResolver.LookupHost(context.Background(), hostname)
	if err != nil {
		return "", err
	}
	if len(addrs) == 0 {
		return "", fmt.Errorf("no addresses found for %s", hostname)
	}
	dnsCache.Store(hostname, addrs[0])
	return addrs[0], nil
}

func createRelayTransport() *http.Transport {
	return &http.Transport{
		TLSClientConfig: &tls.Config{
			NextProtos: []string{"http/1.1"},
		},
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			host, port, err := net.SplitHostPort(addr)
			if err != nil {
				return (&net.Dialer{}).DialContext(ctx, network, addr)
			}
			if host == "127.0.0.1" || host == "localhost" {
				return (&net.Dialer{}).DialContext(ctx, network, addr)
			}
			resolvedIP, err := resolveViaDoH(host)
			if err != nil {
				return (&net.Dialer{}).DialContext(ctx, network, addr)
			}
			return (&net.Dialer{}).DialContext(ctx, network, net.JoinHostPort(resolvedIP, port))
		},
	}
}

var (
	titleStyle = lipgloss.NewStyle().
			Bold(true).
			Foreground(lipgloss.Color("212")).
			Border(lipgloss.RoundedBorder()).
			BorderForeground(lipgloss.Color("62")).
			Padding(0, 2)

	selectedStyle = lipgloss.NewStyle().
			Foreground(lipgloss.Color("212")).
			Bold(true)

	normalStyle = lipgloss.NewStyle().
			Foreground(lipgloss.Color("252"))

	dimStyle = lipgloss.NewStyle().
			Foreground(lipgloss.Color("240"))
)

type menuModel struct {
	choices  []string
	cursor   int
	auth     *AuthConfig
	device   *DeviceConfig
	selected string
	quitting bool
}

func initialMenu() menuModel {
	auth, _ := loadAuthConfig()
	device, _ := loadDeviceConfig()

	var choices []string
	if auth == nil {
		choices = []string{"Login", "Exit"}
	} else if device == nil {
		choices = []string{"Start Tunnel", "Logout", "Exit"}
	} else {
		choices = []string{"Start Tunnel", "Re-pair Device", "Logout", "Exit"}
	}

	return menuModel{
		choices: choices,
		cursor:  0,
		auth:    auth,
		device:  device,
	}
}

func (m menuModel) Init() tea.Cmd {
	return nil
}

func (m menuModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.KeyMsg:
		switch msg.String() {
		case "ctrl+c", "q", "esc":
			m.quitting = true
			return m, tea.Quit
		case "up", "k":
			if m.cursor > 0 {
				m.cursor--
			}
		case "down", "j":
			if m.cursor < len(m.choices)-1 {
				m.cursor++
			}
		case "enter":
			m.selected = m.choices[m.cursor]
			return m, tea.Quit
		}
	}
	return m, nil
}

func (m menuModel) View() string {
	var s strings.Builder

	s.WriteString("\n")
	s.WriteString(titleStyle.Render("  OpenCode Anywhere"))
	s.WriteString("\n\n")

	if m.auth != nil {
		s.WriteString(fmt.Sprintf("  Logged in as: %s\n", m.auth.Email))
		if m.device != nil {
			s.WriteString(fmt.Sprintf("  Device: %s ✓\n", m.device.DeviceName))
		} else {
			s.WriteString("  Device: Not paired\n")
		}
	} else {
		s.WriteString("  Not logged in\n")
	}
	s.WriteString("\n")

	for i, choice := range m.choices {
		cursor := "  "
		style := normalStyle
		if m.cursor == i {
			cursor = "> "
			style = selectedStyle
		}
		s.WriteString(cursor + style.Render(choice) + "\n")
	}

	s.WriteString("\n")
	s.WriteString(dimStyle.Render("  (↑/↓ navigate, Enter select, q quit)"))
	s.WriteString("\n")

	return s.String()
}

func runMenu() {
	m := initialMenu()
	p := tea.NewProgram(m)

	finalModel, err := p.Run()
	if err != nil {
		log.Fatalf("Error running menu: %v", err)
	}

	result := finalModel.(menuModel)
	if result.quitting {
		return
	}

	switch result.selected {
	case "Start Tunnel":
		cmdStart()
	case "Login":
		doLogin(defaultRelay)
		runMenu()
	case "Re-pair Device":
		cmdPair()
		cmdStart()
	case "Logout":
		cmdLogout()
		runMenu()
	case "Exit":
		return
	}
}

func main() {
	initDebugLogger()
	if len(os.Args) < 2 {
		if term.IsTerminal(int(os.Stdin.Fd())) {
			runMenu()
		} else {
			cmdStart()
		}
		return
	}

	switch os.Args[1] {
	case "start":
		cmdStart()
	case "status":
		cmdStatus()
	case "logout":
		cmdLogout()
	case "pair":
		cmdPair()
	case "discover":
		cmdDiscover()
	case "help", "-h", "--help":
		printUsage()
	default:
		fmt.Printf("Unknown command: %s\n", os.Args[1])
		printUsage()
		os.Exit(1)
	}
}

func printUsage() {
	fmt.Printf(`OpenCode Anywhere

Usage:
  tunnel-client [command] [options]

Commands:
  start     Start the tunnel (default, can be omitted)
  pair      Re-pair device (regenerate QR, keep login)
  status    Show current status
  discover  Discover running opencode instances
  logout    Logout and clear credentials

Options:
  -port <port>          Local OpenCode port (default: %s)
  -relay <url>          Relay server URL (default: %s)
  --name <name>         Host display name (default: OS hostname)
  --token <token>       Relay API token (overrides saved credentials)
  --email <email>       Account email for login
  --password <pass>     Account password for login
  --reset               Discard local identity and register again
  --attach              Reuse a running instance (with --port)
  --install             Register the host, wrap the E2E key, then enable autostart
  --passphrase <pass>   E2E key passphrase (never stored)
  --rewrap              Re-wrap the E2E key with a new passphrase
`, defaultPort, defaultRelay)
}

func doLogin(relay string) *AuthConfig {
	fmt.Println("\n┌─────────────────────────────────────────────┐")
	fmt.Println("│  Login to OpenCode Anywhere                 │")
	fmt.Println("└─────────────────────────────────────────────┘")
	fmt.Println()

	fmt.Print("Email: ")
	var email string
	fmt.Scanln(&email)

	fmt.Print("Password: ")
	passwordBytes, err := term.ReadPassword(int(syscall.Stdin))
	fmt.Println()
	if err != nil {
		log.Fatalf("Failed to read password: %v", err)
	}
	password := string(passwordBytes)

	token, userEmail, err := login(relay, email, password)
	if err != nil {
		log.Fatalf("Login failed: %v", err)
	}

	auth := &AuthConfig{
		RelayURL: relay,
		Token:    token,
		Email:    userEmail,
	}
	if err := saveAuthConfig(auth); err != nil {
		log.Fatalf("Failed to save credentials: %v", err)
	}

	fmt.Printf("✓ Logged in as %s\n\n", userEmail)
	return auth
}

type startOptions struct {
	Port       string
	Relay      string
	Name       string
	Token      string
	Email      string
	Password   string
	Passphrase string
	Rewrap     bool
	Reset      bool
	Attach     bool
	Install    bool
}

func parseStartArgs(args []string) startOptions {
	opts := startOptions{Relay: defaultRelay}
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "-port", "--port":
			if i+1 < len(args) {
				opts.Port = args[i+1]
				i++
			}
		case "-relay", "--relay":
			if i+1 < len(args) {
				opts.Relay = args[i+1]
				i++
			}
		case "--name":
			if i+1 < len(args) {
				opts.Name = args[i+1]
				i++
			}
		case "--token":
			if i+1 < len(args) {
				opts.Token = args[i+1]
				i++
			}
		case "--email":
			if i+1 < len(args) {
				opts.Email = args[i+1]
				i++
			}
		case "--password":
			if i+1 < len(args) {
				opts.Password = args[i+1]
				i++
			}
		case "--passphrase":
			if i+1 < len(args) {
				opts.Passphrase = args[i+1]
				i++
			}
		case "--rewrap":
			opts.Rewrap = true
		case "--reset":
			opts.Reset = true
		case "--attach":
			opts.Attach = true
		case "--install":
			opts.Install = true
		}
	}
	return opts
}

// resolveAuth applies the credential priority:
// --token > OPENCODE_RELAY_TOKEN > auth.json > --email/--password login.
func resolveAuth(relay string, opts startOptions) (*AuthConfig, error) {
	if opts.Token != "" {
		return &AuthConfig{RelayURL: relay, Token: opts.Token}, nil
	}
	if token := os.Getenv("OPENCODE_RELAY_TOKEN"); token != "" {
		return &AuthConfig{RelayURL: relay, Token: token}, nil
	}
	if auth, err := loadAuthConfig(); err == nil && auth.Token != "" {
		return auth, nil
	}
	if opts.Email == "" && opts.Password == "" {
		return nil, fmt.Errorf("no credentials: use --token, OPENCODE_RELAY_TOKEN, or --email/--password")
	}
	if opts.Email == "" || opts.Password == "" {
		return nil, fmt.Errorf("--email and --password must be provided together")
	}

	token, email, err := login(relay, opts.Email, opts.Password)
	if err != nil {
		return nil, err
	}
	auth := &AuthConfig{RelayURL: relay, Token: token, Email: email}
	if err := saveAuthConfig(auth); err != nil {
		return nil, err
	}
	return auth, nil
}

// tunnelRunner is a seam so tests can drive cmdStart's failure branches without
// opening a real tunnel.
var tunnelRunner = runTunnel

// fatalExit is a seam so tests can observe terminal failure branches without
// terminating the test process.
var fatalExit = func(format string, v ...interface{}) {
	log.Fatalf(format, v...)
}

func cmdStart() {
	fmt.Printf("tunnel-client %s (built: %s, commit: %s)\n", buildVersion, buildTime, buildCommit)
	opts := parseStartArgs(os.Args[1:])

	localPort := opts.Port
	if localPort == "" {
		localPort = defaultPort
		if ocConfig, _ := loadOpenCodeConfig(); ocConfig != nil && ocConfig.Port != "" {
			localPort = ocConfig.Port
		}
	}

	relay := opts.Relay
	name := opts.Name
	if name == "" {
		name, _ = os.Hostname()
	}
	if name == "" {
		name = "OpenCode Host"
	}

	// --install only validates the identity and registers autostart; it never
	// enters the tunnel loop.
	if opts.Install {
		if err := installAutoStart(opts, relay, name, localPort); err != nil {
			log.Fatalf("--install 失败: %v", err)
		}
		return
	}

	if err := prepareLocalServer(opts, localPort); err != nil {
		log.Fatalf("%v", err)
	}

	var auth *AuthConfig
	reset := opts.Reset
	rewrap := opts.Rewrap
	needKeySetup := true
	warnedNoPassphrase := false
	reregistered := false
	backoff := time.Second
	const maxBackoff = 10 * time.Second

	for {
		device := loadExistingDevice(reset)
		if device == nil {
			if auth == nil {
				var err error
				auth, err = resolveAuth(relay, opts)
				if err != nil {
					log.Fatalf("Failed to resolve credentials: %v", err)
				}
				if relay == defaultRelay && auth.RelayURL != "" {
					relay = auth.RelayURL
				}
			}
			var err error
			device, err = ensureHost(relay, auth.Token, name, reset)
			if err != nil {
				log.Fatalf("Failed to register host: %v", err)
			}
		}
		reset = false

		if needKeySetup {
			token := ""
			if auth != nil {
				token = auth.Token
			} else if resolved, authErr := resolveAuth(relay, opts); authErr == nil {
				auth = resolved
				token = resolved.Token
			}
			if token == "" {
				fmt.Println("  ⚠ 未找到可用令牌，跳过 E2E 密钥上报；隧道仍可建立。")
			} else if err := prepareHostKey(relay, token, device, opts.Passphrase, rewrap); errors.Is(err, errNoPassphrase) && !warnedNoPassphrase {
				fmt.Println("  ⚠ 未提供 E2E 口令（--passphrase / OPENCODE_TUNNEL_PASSPHRASE），跳过密钥上报；隧道仍可建立。")
				warnedNoPassphrase = true
			}
			rewrap = false
			needKeySetup = false
		}

		fmt.Printf("  Using device: %s\n", device.DeviceName)
		fmt.Printf("  Connecting to relay server...\n")
		err := tunnelRunner(device, localPort)
		if err == nil {
			return
		}

		switch classifyFailure(err) {
		case failureCredential:
			fmt.Println("\n  ✗ 凭据已失效，请重新登录或使用 --reset")
			fatalExit("凭据已失效，请重新登录或使用 --reset")
		case failureProtocol:
			fatalExit("协议版本不匹配：请升级 tunnel-client 或 Relay 服务端（%v）", err)
		case failurePolicy:
			fmt.Printf("\n  ✗ 服务端拒绝连接: %v\n", err)
			fatalExit("服务端拒绝连接，已停止重连（%v）", err)
		case failureNotFound:
			if reregistered {
				fmt.Println("\n  ✗ 重新注册后主机仍不存在，凭据可能已失效")
				fatalExit("凭据已失效，请重新登录或使用 --reset")
			}
			fmt.Println("\n  ⚠ 主机不存在，正在使用本地凭据重新注册...")
			if auth == nil {
				var authErr error
				auth, authErr = resolveAuth(relay, opts)
				if authErr != nil {
					log.Fatalf("Failed to resolve credentials: %v", authErr)
				}
			}
			reuseName := device.HostName
			if reuseName == "" {
				reuseName = name
			}
			// Re-registration must keep the existing E2E material: the wrapped
			// copy is still valid for the same local host key, and losing it
			// would force an unnecessary re-wrap (and a passphrase prompt).
			prevKey := device.EncryptionKey
			prevWrapped, prevSalt, prevKdf := device.KeyWrapped, device.KeySalt, device.KeyKdf
			refreshed, err := ensureHost(relay, auth.Token, reuseName, false)
			if err != nil {
				fmt.Printf("\n  ✗ 重新注册失败: %v\n", err)
				log.Fatalf("凭据已失效，请重新登录或使用 --reset")
			}
			refreshed.EncryptionKey = prevKey
			refreshed.KeyWrapped, refreshed.KeySalt, refreshed.KeyKdf = prevWrapped, prevSalt, prevKdf
			if err := saveDeviceConfig(refreshed); err != nil {
				log.Fatalf("Failed to save device config: %v", err)
			}
			reregistered = true
			needKeySetup = true
			continue
		default:
			delay := backoffDelay(backoff)
			fmt.Printf("\n  ⚠ 网络错误: %v，%v 后重连\n", err, delay.Round(time.Millisecond))
			time.Sleep(delay)
			backoff *= 2
			if backoff > maxBackoff {
				backoff = maxBackoff
			}
		}
	}
}

// probeLocalServer reports whether something already answers on the local port.
// It is a package-level var so tests can stub the network probe.
var probeLocalServer = func(port string) error {
	client := &http.Client{Timeout: 2 * time.Second}
	// Use 127.0.0.1 instead of localhost to avoid IPv6 timeout issues
	url := fmt.Sprintf("http://127.0.0.1:%s", port)
	resp, err := client.Get(url)
	if err != nil {
		return err
	}
	resp.Body.Close()
	return nil
}

// prepareLocalServer decides how the local OpenCode instance is obtained:
// --attach reuses one that must already be listening, otherwise the existing
// detect-and-spawn logic runs.
func prepareLocalServer(opts startOptions, localPort string) error {
	if opts.Attach {
		if err := probeLocalServer(localPort); err != nil {
			debugLog("[attach] probe failed on port %s: %v", localPort, err)
			return fmt.Errorf("--attach 指定端口 %s 无响应，请先启动 opencode serve", localPort)
		}
		debugLog("[attach] reusing OpenCode already listening on port %s", localPort)
		return nil
	}
	ensureOpenCodeRunning(localPort)
	return nil
}

// installAutoStart makes sure the host is registered with wrapped E2E key
// material, then installs the OS autostart entry. It never starts the tunnel,
// and it fails loudly when key wrapping needs a passphrase it cannot obtain.
func installAutoStart(opts startOptions, relay, name, localPort string) error {
	device := loadExistingDevice(opts.Reset)
	// Key setup is needed when the identity or key material is incomplete, and
	// also when --rewrap explicitly asks for fresh wrapped material even though
	// the existing identity is complete.
	needsKeySetup := device == nil || device.HostID == "" ||
		device.KeyWrapped == "" || device.KeySalt == "" || device.KeyKdf == "" ||
		opts.Rewrap
	if needsKeySetup {
		auth, err := resolveAuth(relay, opts)
		if err != nil {
			return fmt.Errorf("resolve credentials: %w", err)
		}
		if relay == defaultRelay && auth.RelayURL != "" {
			relay = auth.RelayURL
		}
		if device == nil || device.HostID == "" {
			device, err = ensureHost(relay, auth.Token, name, opts.Reset)
			if err != nil {
				return fmt.Errorf("register host: %w", err)
			}
		}
		if err := prepareHostKey(relay, auth.Token, device, opts.Passphrase, opts.Rewrap); err != nil {
			return err
		}
	}

	if err := setupAutoStart(localPort); err != nil {
		return fmt.Errorf("setup autostart: %w", err)
	}
	fmt.Printf("✓ 已配置开机自启（端口 %s）\n", localPort)
	return nil
}

func cmdStatus() {
	auth, authErr := loadAuthConfig()
	device, deviceErr := loadDeviceConfig()

	if authErr != nil && deviceErr != nil {
		fmt.Println("Not logged in")
		return
	}

	if auth != nil {
		fmt.Printf("Logged in as: %s\n", auth.Email)
		fmt.Printf("Relay: %s\n", auth.RelayURL)
	}

	if device != nil {
		fmt.Printf("\nDevice: %s\n", device.DeviceName)
		fmt.Printf("Host ID: %s\n", device.HostID)
		fmt.Printf("Subdomain: %s\n", device.Subdomain)
		if device.KeyWrapped != "" {
			fmt.Println("E2E key: present")
		} else {
			fmt.Println("E2E key: missing")
		}
	} else {
		fmt.Println("\nNo device paired")
	}
}

func cmdLogout() {
	configPath := getConfigDir()

	authPath := filepath.Join(configPath, authFileName)
	os.Remove(authPath)

	devicePath := filepath.Join(configPath, deviceFileName)
	os.Remove(devicePath)

	fmt.Println("✓ Logged out")
}

func cmdPair() {
	relay := defaultRelay
	localPort := defaultPort

	for i := 1; i < len(os.Args); i++ {
		switch os.Args[i] {
		case "-port":
			if i+1 < len(os.Args) {
				localPort = os.Args[i+1]
				i++
			}
		case "-relay":
			if i+1 < len(os.Args) {
				relay = os.Args[i+1]
				i++
			}
		}
	}

	auth, err := loadAuthConfig()
	if err != nil {
		auth = doLogin(relay)
	}

	if relay == defaultRelay && auth.RelayURL != "" {
		relay = auth.RelayURL
	}

	clearDeviceConfig()

	device, err := startPairing(relay, auth.Token, localPort)
	if err != nil {
		log.Fatalf("Pairing failed: %v", err)
	}

	device.RelayURL = relay
	if err := saveDeviceConfig(device); err != nil {
		log.Fatalf("Failed to save device config: %v", err)
	}

	fmt.Printf("\n✓ Device \"%s\" paired successfully!\n", device.DeviceName)
	setupAutoStart(localPort)
}

// ensureOpenCodeRunning is a package-level var so tests can stub the spawn path.
var ensureOpenCodeRunning = func(port string) {
	client := &http.Client{Timeout: 2 * time.Second}
	// Use 127.0.0.1 instead of localhost to avoid IPv6 timeout issues
	url := fmt.Sprintf("http://127.0.0.1:%s", port)

	// Retry up to 3 times with 1 second interval
	for attempt := 1; attempt <= 3; attempt++ {
		resp, err := client.Get(url)
		if err == nil {
			resp.Body.Close()
			debugLog("[detection] OpenCode detected on port %s (attempt %d)", port, attempt)
			return
		}

		debugLog("[detection] Attempt %d: failed to reach %s: %v", attempt, url, err)

		if attempt < 3 {
			time.Sleep(1 * time.Second)
		}
	}

	// HTTP detection failed, check if opencode process is running
	if pids := getOpenCodePIDs(); len(pids) > 0 {
		debugLog("[detection] HTTP check failed but opencode process found: PIDs %v", pids)
		fmt.Printf("  OpenCode process found (PID: %s), waiting for port %s...\n", strings.Join(pids, ", "), port)
		// Wait for port to be ready (up to 30 seconds)
		for i := 0; i < 30; i++ {
			time.Sleep(1 * time.Second)
			resp, err := client.Get(url)
			if err == nil {
				resp.Body.Close()
				fmt.Printf("  ✓ OpenCode is ready on port %s\n", port)
				return
			}
		}
		fmt.Printf("  OpenCode process exists but port %s not responding\n", port)
	}

	fmt.Printf("  OpenCode not detected on port %s\n", port)

	ocConfig, _ := loadOpenCodeConfig()
	if ocConfig != nil && ocConfig.AutoStart && ocConfig.Command != "" {
		fmt.Printf("  Starting OpenCode...\n")
		if startOpenCode(ocConfig, port) {
			return
		}
	}

	configureAndStartOpenCode(port, client)
}

func getOpenCodePIDs() []string {
	var pids []string

	if runtime.GOOS == "windows" {
		cmd := exec.Command("tasklist", "/FI", "IMAGENAME eq opencode.exe", "/FO", "CSV", "/NH")
		output, err := cmd.Output()
		if err != nil {
			return nil
		}
		// Parse CSV output: "opencode.exe","1234",...
		lines := strings.Split(strings.TrimSpace(string(output)), "\n")
		for _, line := range lines {
			parts := strings.Split(line, ",")
			if len(parts) >= 2 {
				pid := strings.Trim(parts[1], "\"")
				pids = append(pids, pid)
			}
		}
	} else {
		// Use ps + grep for cross-platform compatibility (pgrep behaves differently on macOS)
		cmd := exec.Command("sh", "-c", "ps -eo pid,comm | grep '[o]pencode$' | awk '{print $1}'")
		output, err := cmd.Output()
		if err != nil {
			return nil
		}
		lines := strings.Split(strings.TrimSpace(string(output)), "\n")
		for _, line := range lines {
			if pid := strings.TrimSpace(line); pid != "" {
				pids = append(pids, pid)
			}
		}
	}

	return pids
}

func configureAndStartOpenCode(port string, client *http.Client) {
	command := detectOpenCodeCommand()
	if command == "" {
		fmt.Println("  OpenCode not found. Please enter the command to start it")
		fmt.Print("  (e.g., 'opencode', 'npx opencode'): ")
		fmt.Scanln(&command)
		if command == "" {
			fmt.Println("  Waiting for OpenCode to start manually...")
			waitForOpenCodeManually(port, client)
			return
		}
	}

	cwd, _ := os.Getwd()
	fmt.Printf("  Working directory [%s]: ", cwd)
	var workdir string
	fmt.Scanln(&workdir)
	if workdir == "" {
		workdir = cwd
	}

	if strings.HasPrefix(workdir, "~") {
		home, _ := os.UserHomeDir()
		workdir = filepath.Join(home, strings.TrimPrefix(workdir[1:], "/"))
	}

	if _, err := os.Stat(workdir); os.IsNotExist(err) {
		fmt.Printf("  Directory does not exist: %s\n", workdir)
		fmt.Println("  Waiting for OpenCode to start manually...")
		waitForOpenCodeManually(port, client)
		return
	}

	ocConfig := &OpenCodeConfig{
		Command:   command,
		WorkDir:   workdir,
		Port:      port,
		AutoStart: true,
	}
	saveOpenCodeConfig(ocConfig)

	if !startOpenCode(ocConfig, port) {
		fmt.Println("  Waiting for OpenCode to start manually...")
		waitForOpenCodeManually(port, client)
	}
}

func detectOpenCodeCommand() string {
	commands := []string{"opencode", "npx opencode"}

	for _, cmd := range commands {
		parts := strings.Fields(cmd)
		path, err := exec.LookPath(parts[0])
		if err == nil && path != "" {
			return cmd
		}
	}
	return ""
}

func startOpenCode(config *OpenCodeConfig, port string) bool {
	fmt.Printf("  Starting OpenCode in %s...\n", config.WorkDir)

	parts := strings.Fields(config.Command)
	if len(parts) == 0 {
		return false
	}

	args := parts[1:]
	args = append(args, "--port", port)

	cmd := exec.Command(parts[0], args...)
	cmd.Dir = config.WorkDir
	setProcAttr(cmd)

	logDir := filepath.Join(getConfigDir())
	os.MkdirAll(logDir, 0755)
	logFile, err := os.OpenFile(filepath.Join(logDir, "opencode.log"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
	if err == nil {
		cmd.Stdout = logFile
		cmd.Stderr = logFile
	}

	if err := cmd.Start(); err != nil {
		fmt.Printf("  Failed to start OpenCode: %v\n", err)
		return false
	}

	fmt.Printf("  OpenCode starting (PID: %d)...\n", cmd.Process.Pid)

	// Reap the child process to prevent zombies
	go func() {
		cmd.Wait()
		if logFile != nil {
			logFile.Close()
		}
	}()

	client := &http.Client{Timeout: 2 * time.Second}
	url := fmt.Sprintf("http://127.0.0.1:%s", port)

	for i := 0; i < 30; i++ {
		time.Sleep(time.Second)
		resp, err := client.Get(url)
		if err == nil {
			resp.Body.Close()
			fmt.Println("  ✓ OpenCode started successfully!")
			return true
		}
	}

	fmt.Println("  OpenCode did not start in time.")
	return false
}

func waitForOpenCodeManually(port string, client *http.Client) {
	url := fmt.Sprintf("http://127.0.0.1:%s", port)
	ticker := time.NewTicker(3 * time.Second)
	defer ticker.Stop()

	for range ticker.C {
		resp, err := client.Get(url)
		if err == nil {
			resp.Body.Close()
			fmt.Println("✓ OpenCode detected!")
			return
		}
	}
}

func loadOpenCodeConfig() (*OpenCodeConfig, error) {
	path := filepath.Join(getConfigDir(), opencodeFileName)
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var config OpenCodeConfig
	if err := json.Unmarshal(data, &config); err != nil {
		return nil, err
	}
	return &config, nil
}

func saveOpenCodeConfig(config *OpenCodeConfig) error {
	dir := getConfigDir()
	if err := os.MkdirAll(dir, 0700); err != nil {
		return err
	}
	data, _ := json.MarshalIndent(config, "", "  ")
	return os.WriteFile(filepath.Join(dir, opencodeFileName), data, 0600)
}

func login(relayURL, email, password string) (string, string, error) {
	body, _ := json.Marshal(map[string]string{
		"email":    email,
		"password": password,
	})

	client := &http.Client{Timeout: 10 * time.Second, Transport: createRelayTransport()}
	resp, err := client.Post(relayURL+"/api/login", "application/json", bytes.NewReader(body))
	if err != nil {
		return "", "", err
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		bodyBytes, _ := io.ReadAll(resp.Body)
		return "", "", fmt.Errorf("login failed: %s", string(bodyBytes))
	}

	var result LoginResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return "", "", err
	}

	return result.Token, result.User.Email, nil
}

type deviceRecord struct {
	ID           int64  `json:"id"`
	Name         string `json:"name"`
	Subdomain    string `json:"subdomain"`
	AuthUser     string `json:"auth_user"`
	AuthPassword string `json:"auth_password"`
}

func listDevices(relay, token string) ([]deviceRecord, error) {
	req, err := http.NewRequest("GET", relay+"/api/devices", nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+token)

	client := &http.Client{Timeout: 10 * time.Second, Transport: createRelayTransport()}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		bodyBytes, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("failed to list devices: %s", string(bodyBytes))
	}

	var devices []deviceRecord
	if err := json.NewDecoder(resp.Body).Decode(&devices); err != nil {
		return nil, err
	}
	return devices, nil
}

func deviceConfigFromRecord(d deviceRecord, relay string) *DeviceConfig {
	return &DeviceConfig{
		DeviceID:     d.ID,
		DeviceName:   d.Name,
		RelayURL:     relay,
		Subdomain:    d.Subdomain,
		AuthUser:     d.AuthUser,
		AuthPassword: d.AuthPassword,
		HostID:       d.Subdomain,
		HostName:     d.Name,
	}
}

// registerHost reuses the device already registered under name, or creates it.
func registerHost(relay, token, name string) (*DeviceConfig, error) {
	existing, err := findDeviceByName(relay, token, name)
	if err != nil {
		return nil, err
	}
	if existing != nil {
		return deviceConfigFromRecord(*existing, relay), nil
	}

	body, _ := json.Marshal(map[string]string{"name": name})
	req, err := http.NewRequest("POST", relay+"/api/devices", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")

	client := &http.Client{Timeout: 10 * time.Second, Transport: createRelayTransport()}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != 201 {
		bodyBytes, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("failed to register host: %s", string(bodyBytes))
	}

	var created deviceRecord
	if err := json.NewDecoder(resp.Body).Decode(&created); err != nil {
		return nil, err
	}
	return deviceConfigFromRecord(created, relay), nil
}

func findDeviceByName(relay, token, name string) (*deviceRecord, error) {
	devices, err := listDevices(relay, token)
	if err != nil {
		return nil, err
	}
	for i := range devices {
		if devices[i].Name == name {
			return &devices[i], nil
		}
	}
	return nil, nil
}

// ensureHost registers the host and persists the identity to device.json.
func ensureHost(relay, token, name string, reset bool) (*DeviceConfig, error) {
	if reset {
		clearDeviceConfig()
		if existing, err := findDeviceByName(relay, token, name); err == nil && existing != nil {
			return nil, fmt.Errorf("同名主机已存在，请用 --name 区分")
		}
	}

	// A local identity already bound to a HostID must not silently adopt a
	// same-name server record that points at a different device.
	if local, err := loadDeviceConfig(); err == nil && local.HostID != "" {
		existing, err := findDeviceByName(relay, token, name)
		if err != nil {
			return nil, err
		}
		if existing != nil && existing.Subdomain != local.HostID {
			return nil, fmt.Errorf("同名主机已绑定其他设备，请用 --name 区分")
		}
	}

	device, err := registerHost(relay, token, name)
	if err != nil {
		return nil, err
	}
	if err := saveDeviceConfig(device); err != nil {
		return nil, err
	}
	return device, nil
}

// loadExistingDevice returns the saved identity unless --reset discards it.
func loadExistingDevice(reset bool) *DeviceConfig {
	if reset {
		return nil
	}
	device, err := loadDeviceConfig()
	if err != nil {
		return nil
	}
	return device
}

func startPairing(relayURL, token, localPort string) (*DeviceConfig, error) {
	req, _ := http.NewRequest("POST", relayURL+"/api/pairing", nil)
	req.Header.Set("Authorization", "Bearer "+token)

	client := &http.Client{Timeout: 10 * time.Second, Transport: createRelayTransport()}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != 201 {
		bodyBytes, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("failed to create pairing: %s", string(bodyBytes))
	}

	var pairing PairingResponse
	if err := json.NewDecoder(resp.Body).Decode(&pairing); err != nil {
		return nil, err
	}

	hostname, _ := os.Hostname()
	encryptionKey, err := generateEncryptionKey()
	if err != nil {
		return nil, fmt.Errorf("failed to generate encryption key: %w", err)
	}

	qrData := QRData{
		Version:       1,
		RelayURL:      relayURL,
		PairingID:     pairing.ID,
		PairingCode:   pairing.PairingCode,
		Hostname:      hostname,
		EncryptionKey: encryptionKey,
	}
	qrJSON, _ := json.Marshal(qrData)

	qr, err := qrcode.New(string(qrJSON), qrcode.Medium)
	if err != nil {
		return nil, fmt.Errorf("failed to generate QR code: %w", err)
	}

	fmt.Println("\n┌─────────────────────────────────────────────┐")
	fmt.Println("│  Scan this QR code with the OpenCode App    │")
	fmt.Println("└─────────────────────────────────────────────┘")
	fmt.Println()
	fmt.Println(qr.ToSmallString(false))
	fmt.Printf("  Pairing code: %s\n", pairing.PairingCode)
	fmt.Printf("  Expires at: %s\n\n", pairing.ExpiresAt.Local().Format("15:04:05"))
	fmt.Println("  Waiting for app to scan...")

	device, err := pollPairingStatus(relayURL, token, pairing.ID, pairing.ExpiresAt)
	if err != nil {
		return nil, err
	}
	device.EncryptionKey = encryptionKey
	return device, nil
}

func pollPairingStatus(relayURL, token, pairingID string, expiresAt time.Time) (*DeviceConfig, error) {
	client := &http.Client{Timeout: 10 * time.Second, Transport: createRelayTransport()}

	for {
		if time.Now().After(expiresAt) {
			return nil, fmt.Errorf("pairing expired")
		}

		req, _ := http.NewRequest("GET", relayURL+"/api/pairing/"+pairingID+"/status", nil)
		req.Header.Set("Authorization", "Bearer "+token)

		resp, err := client.Do(req)
		if err != nil {
			time.Sleep(pairingInterval)
			continue
		}

		var status PairingStatusResponse
		json.NewDecoder(resp.Body).Decode(&status)
		resp.Body.Close()

		switch status.Status {
		case "completed":
			if status.Device != nil {
				return status.Device, nil
			}
			return nil, fmt.Errorf("pairing completed but no device info")
		case "expired":
			return nil, fmt.Errorf("pairing expired")
		}

		time.Sleep(pairingInterval)
	}
}

func runTunnel(config *DeviceConfig, localPort string) error {
	if err := verifyDeviceWithServer(config); err != nil {
		return err
	}

	client := &TunnelClient{
		config:    config,
		localPort: localPort,
		httpClient: &http.Client{
			Timeout: 120 * time.Second,
		},
	}
	if ocConfig, err := loadOpenCodeConfig(); err == nil && ocConfig != nil {
		client.localAuthUser = ocConfig.AuthUser
		client.localAuthPassword = ocConfig.AuthPassword
	}

	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP, syscall.SIGQUIT)
	defer signal.Stop(sigChan)

	// The watcher must exit when this attempt ends, otherwise every reconnect
	// leaves a goroutine parked on sigChan forever.
	watcherDone := make(chan struct{})
	defer close(watcherDone)

	go func() {
		select {
		case sig := <-sigChan:
			log.Printf("Received signal: %v (PID: %d)", sig, os.Getpid())
			fmt.Printf("\nShutting down due to signal: %v...\n", sig)
			if client.conn != nil {
				client.conn.Close()
			}
			os.Exit(0)
		case <-watcherDone:
		}
	}()

	return client.connectWithRetry()
}

// verifyDeviceWithServer reports whether the saved identity still exists on the
// relay. HTTP status is carried in the error so classifyFailure can decide.
func verifyDeviceWithServer(config *DeviceConfig) error {
	reqBody, _ := json.Marshal(map[string]string{
		"subdomain":     config.Subdomain,
		"auth_user":     config.AuthUser,
		"auth_password": config.AuthPassword,
	})

	client := &http.Client{Timeout: 10 * time.Second, Transport: createRelayTransport()}
	resp, err := client.Post(config.RelayURL+"/api/device/verify", "application/json", bytes.NewReader(reqBody))
	if err != nil {
		// A network hiccup here is not evidence against the identity; let the
		// WebSocket dial below decide and retry.
		debugLog("[verify] Request failed: %v, proceeding to dial", err)
		return nil
	}
	defer resp.Body.Close()

	if resp.StatusCode == 200 {
		debugLog("[verify] Device verified successfully")
		return nil
	}

	body, _ := io.ReadAll(resp.Body)
	debugLog("[verify] Device verification failed: status=%d, body=%s", resp.StatusCode, string(body))
	return &httpStatusError{Op: "verify", Status: resp.StatusCode}
}

type failureKind int

const (
	failureNetwork failureKind = iota
	failureCredential
	failureNotFound
	failureProtocol
	failurePolicy
)

// httpStatusError keeps an HTTP status on the error so failure classification
// does not have to grep for digits.
type httpStatusError struct {
	Op     string
	Status int
}

func (e *httpStatusError) Error() string {
	return fmt.Sprintf("%s: status %d", e.Op, e.Status)
}

// classifyFailure maps a tunnel failure to the D4 table. Unknown errors are
// treated as network failures because retrying is safe; only credentials stop.
func classifyFailure(err error) failureKind {
	if err == nil {
		return failureNetwork
	}

	var statusErr *httpStatusError
	if errors.As(err, &statusErr) {
		switch {
		case statusErr.Status == http.StatusUnauthorized, statusErr.Status == http.StatusForbidden:
			return failureCredential
		case statusErr.Status == http.StatusNotFound:
			return failureNotFound
		case statusErr.Status >= 500:
			return failureNetwork
		case statusErr.Status >= 400:
			// The server refused the upgrade (e.g. 400/426): version mismatch.
			return failureProtocol
		default:
			return failureNetwork
		}
	}

	var closeErr *websocket.CloseError
	if errors.As(err, &closeErr) {
		switch closeErr.Code {
		case websocket.ClosePolicyViolation, // 1008: server refused the client
			websocket.CloseUnsupportedData: // 1003: server cannot handle the stream
			return failurePolicy
		default:
			// 1000/1001/1006/1011 and unknown codes are transient: reconnect.
			// 1011 in particular means the relay hit an unexpected condition
			// (restart, crash, upgrade), which must not disable a daemon.
			return failureNetwork
		}
	}

	msg := err.Error()
	switch {
	case strings.Contains(msg, "bad handshake"):
		return failureProtocol
	case strings.Contains(msg, "401"), strings.Contains(msg, "403"),
		strings.Contains(msg, "Unauthorized"), strings.Contains(msg, "Forbidden"):
		return failureCredential
	case strings.Contains(msg, "404"), strings.Contains(msg, "not found"):
		return failureNotFound
	}
	return failureNetwork
}

// backoffDelay applies ±20% jitter to d.
func backoffDelay(d time.Duration) time.Duration {
	return time.Duration(float64(d) * (0.8 + 0.4*mrand.Float64()))
}

// connectWithRetry keeps the identity on every failure: network errors retry
// with backoff, everything else is handed back to cmdStart for a decision.
func (c *TunnelClient) connectWithRetry() error {
	backoff := time.Second
	const maxBackoff = 10 * time.Second

	for {
		err := c.connect()
		if err == nil {
			backoff = time.Second
			continue
		}
		if classifyFailure(err) != failureNetwork {
			return err
		}

		delay := backoffDelay(backoff)
		log.Printf("Connection error: %v. Retrying in %v...", err, delay)
		time.Sleep(delay)
		backoff *= 2
		if backoff > maxBackoff {
			backoff = maxBackoff
		}
	}
}

func clearDeviceConfig() {
	path := filepath.Join(getConfigDir(), deviceFileName)
	os.Remove(path)
}

func clearAuthConfig() {
	path := filepath.Join(getConfigDir(), authFileName)
	os.Remove(path)
}

// wsDial is a seam so tests can exercise the failed-handshake cleanup path.
var wsDial = func(d *websocket.Dialer, urlStr string, header http.Header) (*websocket.Conn, *http.Response, error) {
	return d.Dial(urlStr, header)
}

func (c *TunnelClient) connect() error {
	wsURL := c.buildWebSocketURL()

	dialer := websocket.Dialer{
		HandshakeTimeout:  10 * time.Second,
		EnableCompression: false,
		TLSClientConfig: &tls.Config{
			NextProtos: []string{"http/1.1"}, // Force HTTP/1.1, avoid HTTP/2 (Azure ALPN issue)
		},
		NetDialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			host, port, err := net.SplitHostPort(addr)
			if err != nil {
				return (&net.Dialer{}).DialContext(ctx, network, addr)
			}
			if host == "127.0.0.1" || host == "localhost" {
				return (&net.Dialer{}).DialContext(ctx, network, addr)
			}
			resolvedIP, err := resolveViaDoH(host)
			if err != nil {
				return (&net.Dialer{}).DialContext(ctx, network, addr)
			}
			return (&net.Dialer{}).DialContext(ctx, network, net.JoinHostPort(resolvedIP, port))
		},
	}
	// Announce the streaming protocol in the upgrade request. Old relays ignore
	// unknown headers and simply omit the response header, which keeps this
	// client on the legacy path.
	handshakeHeader := http.Header{}
	handshakeHeader.Set("X-Tunnel-Protocol", strconv.Itoa(tunnelProtocolVersion))

	conn, resp, err := wsDial(&dialer, wsURL, handshakeHeader)
	if err != nil {
		if resp != nil {
			// The caller owns the response body when the handshake fails. Drain
			// and close it, otherwise every retry leaks a connection.
			if resp.Body != nil {
				io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
				resp.Body.Close()
			}
			return &httpStatusError{Op: "dial", Status: resp.StatusCode}
		}
		return fmt.Errorf("dial failed: %w", err)
	}
	c.conn = conn
	defer conn.Close()

	// Re-negotiate on every connection: a relay downgrade or rollback must put
	// this client back on the legacy path.
	streaming := false
	if resp != nil {
		if version, err := strconv.Atoi(strings.TrimSpace(resp.Header.Get("X-Tunnel-Protocol"))); err == nil && version >= tunnelProtocolVersion {
			streaming = true
		}
	}
	c.setStreamMode(streaming)
	log.Printf("Tunnel protocol negotiated: streaming=%v", streaming)

	fmt.Printf("✓ Connected! Tunneling to localhost:%s\n", c.localPort)

	conn.SetReadDeadline(time.Now().Add(60 * time.Second))
	conn.SetPongHandler(func(string) error {
		conn.SetReadDeadline(time.Now().Add(60 * time.Second))
		return nil
	})

	c.startSSE()

	done := make(chan struct{})
	defer func() {
		close(done)
		c.stopSSE()
		c.sseWaitGroup.Wait()
		// Streams and local WebSockets belong to this connection: a reconnect
		// must not leave them attached to a dead tunnel.
		c.cancelAllStreams()
		c.closeAllWs()
	}()

	go func() {
		ticker := time.NewTicker(10 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				c.writeMu.Lock()
				err := conn.WriteControl(websocket.PingMessage, nil, time.Now().Add(10*time.Second))
				c.writeMu.Unlock()
				if err != nil {
					return
				}
			case <-done:
				return
			}
		}
	}()

	for {
		conn.SetReadDeadline(time.Now().Add(60 * time.Second))
		_, message, err := conn.ReadMessage()
		if err != nil {
			// Log the specific error type for debugging
			if websocket.IsCloseError(err, websocket.CloseNormalClosure, websocket.CloseGoingAway) {
				log.Printf("[WebSocket] Connection closed normally: %v", err)
			} else if websocket.IsUnexpectedCloseError(err, websocket.CloseNormalClosure, websocket.CloseGoingAway) {
				log.Printf("[WebSocket] Unexpected close: %v", err)
			} else {
				log.Printf("[WebSocket] Read error: %v", err)
			}
			return fmt.Errorf("read error: %w", err)
		}

		var heartbeat HeartbeatMessage
		if err := json.Unmarshal(message, &heartbeat); err == nil && heartbeat.Type == "heartbeat" {
			debugLog("[heartbeat] Received heartbeat ts=%d", heartbeat.Ts)
			ack := HeartbeatMessage{
				Type: "heartbeat_ack",
				Ts:   heartbeat.Ts,
			}
			ackData, _ := json.Marshal(ack)
			c.writeMu.Lock()
			conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
			err := conn.WriteMessage(websocket.TextMessage, ackData)
			c.writeMu.Unlock()
			if err != nil {
				log.Printf("[heartbeat] Failed to send heartbeat_ack: %v", err)
			} else {
				debugLog("[heartbeat] Sent heartbeat_ack ts=%d", heartbeat.Ts)
			}
			continue
		}

		var req TunnelRequest
		switch frameTypeOf(message) {
		case tunnel.FrameWsOpen:
			var open tunnel.WsOpen
			if err := json.Unmarshal(message, &open); err != nil {
				debugLog("[ws] failed to decode ws-open: %v", err)
				continue
			}
			go c.handleWsOpen(&open)
		case tunnel.FrameWsFrame:
			var frame tunnel.WsFrame
			if err := json.Unmarshal(message, &frame); err != nil {
				debugLog("[ws] failed to decode ws-frame: %v", err)
				continue
			}
			c.handleWsFrame(&frame)
		case tunnel.FrameWsClose:
			var frame tunnel.WsClose
			if err := json.Unmarshal(message, &frame); err != nil {
				debugLog("[ws] failed to decode ws-close: %v", err)
				continue
			}
			c.handleWsClose(&frame)
		case tunnel.FrameReqCancel:
			var frame tunnel.ReqCancel
			if err := json.Unmarshal(message, &frame); err != nil {
				debugLog("[stream] failed to decode req-cancel: %v", err)
				continue
			}
			c.handleReqCancel(&frame)
		case "":
			if err := json.Unmarshal(message, &req); err != nil {
				continue
			}
			go c.handleRequest(&req)
		default:
			// Unknown frames are ignored so a newer relay can add types without
			// breaking this client (design D1).
			debugLog("[tunnel] ignoring unknown frame type %q", frameTypeOf(message))
		}
	}
}

// frameTypeOf reads only the type discriminator of a tunnel frame. An empty
// type means a legacy frame.
func frameTypeOf(message []byte) string {
	var probe struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal(message, &probe); err != nil {
		return ""
	}
	return probe.Type
}

func (c *TunnelClient) buildWebSocketURL() string {
	u, _ := url.Parse(c.config.RelayURL)

	if u.Scheme == "https" {
		u.Scheme = "wss"
	} else if u.Scheme == "http" {
		u.Scheme = "ws"
	}

	u.Path = fmt.Sprintf("/api/tunnel/%s", c.config.Subdomain)
	q := u.Query()
	q.Set("auth_user", c.config.AuthUser)
	q.Set("auth_password", c.config.AuthPassword)
	u.RawQuery = q.Encode()

	// Never log the full URL: it carries auth_password in the query string.
	debugLog("[debug] WebSocket URL: %s://%s%s (subdomain: %s)", u.Scheme, u.Host, u.Path, c.config.Subdomain)
	return u.String()
}

// startSSE launches the local event-stream subscription for this tunnel
// connection. Each connection gets a fresh stop channel.
func (c *TunnelClient) startSSE() {
	stop := make(chan struct{})

	c.sseStopMu.Lock()
	c.sseStopChan = stop
	c.sseStopped = false
	c.sseStopMu.Unlock()

	c.sseWaitGroup.Add(1)
	go func() {
		// Done lives with the launcher so callers can register their own
		// completion signal before it, guaranteeing it runs first.
		defer c.sseWaitGroup.Done()
		c.subscribeSSE(stop)
	}()
}

// stopSSE closes the subscription stop channel exactly once. It never blocks,
// so it is safe to call from both connect()'s teardown and Stop().
func (c *TunnelClient) stopSSE() {
	c.sseStopMu.Lock()
	ch := c.sseStopChan
	if ch == nil || c.sseStopped {
		c.sseStopMu.Unlock()
		return
	}
	c.sseStopped = true
	c.sseStopMu.Unlock()

	close(ch)
}

// Stop ends the event-stream subscription and waits for it to exit. A
// re-registration builds a new TunnelClient, so the previous instance must be
// stopped before its events are allowed to keep flowing.
func (c *TunnelClient) Stop() {
	c.stopSSE()
	c.sseWaitGroup.Wait()
}

func (c *TunnelClient) subscribeSSE(stop <-chan struct{}) {
	sseURL := fmt.Sprintf("http://127.0.0.1:%s/event", c.localPort)
	debugLog("[SSE] Subscribing to %s", sseURL)

	for {
		select {
		case <-stop:
			debugLog("[SSE] Stopping subscription")
			return
		default:
		}

		err := c.connectSSE(stop, sseURL)
		if err == nil {
			// connectSSE only reports success when the subscription stopped.
			debugLog("[SSE] Subscription stopped")
			return
		}

		debugLog("[SSE] Connection error: %v, reconnecting in %v...", err, sseReconnectDelay)
		select {
		case <-stop:
			return
		case <-time.After(sseReconnectDelay):
		}
	}
}

func (c *TunnelClient) connectSSE(stop <-chan struct{}, sseURL string) error {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// A read parked on an idle stream must not outlive the stop signal,
	// otherwise Stop() would block until the local server happens to speak.
	watcherDone := make(chan struct{})
	defer close(watcherDone)
	go func() {
		select {
		case <-stop:
			cancel()
		case <-watcherDone:
		}
	}()

	req, err := http.NewRequestWithContext(ctx, "GET", sseURL, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "text/event-stream")
	req.Header.Set("Cache-Control", "no-cache")

	client := &http.Client{Timeout: 0}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		return fmt.Errorf("SSE returned status %d", resp.StatusCode)
	}

	debugLog("[SSE] Connected to OpenCode events")

	reader := resp.Body
	buf := make([]byte, 0, 4096)
	tmp := make([]byte, 1024)

	for {
		select {
		case <-stop:
			return nil
		default:
		}

		n, readErr := reader.Read(tmp)
		if n > 0 {
			// Process whatever arrived before honouring the error: a stream that
			// closes right after its last event returns data and io.EOF in the
			// same call, and dropping that chunk loses the final event.
			buf = append(buf, tmp[:n]...)

			for {
				idx := bytes.Index(buf, []byte("\n\n"))
				if idx == -1 {
					break
				}

				eventData := buf[:idx]
				buf = buf[idx+2:]

				c.processSSEEvent(eventData)
			}
		}

		if readErr != nil {
			if readErr == io.EOF {
				return fmt.Errorf("SSE connection closed")
			}
			return readErr
		}
	}
}

func (c *TunnelClient) processSSEEvent(eventData []byte) {
	lines := bytes.Split(eventData, []byte("\n"))
	var dataLines [][]byte

	for _, line := range lines {
		if bytes.HasPrefix(line, []byte("data:")) {
			value := bytes.TrimPrefix(line, []byte("data:"))
			// SSE strips a single optional space after the colon.
			value = bytes.TrimPrefix(value, []byte(" "))
			dataLines = append(dataLines, value)
		}
	}

	if len(dataLines) == 0 {
		return
	}

	// Multiple data: lines belong to one event; the spec joins them with "\n".
	data := bytes.TrimSpace(bytes.Join(dataLines, []byte("\n")))
	if len(data) == 0 {
		return
	}

	var raw struct {
		Payload json.RawMessage `json:"payload"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		debugLog("[SSE] Failed to parse event: %v", err)
		return
	}

	payload := raw.Payload
	if payload == nil {
		payload = data
	}

	var event SSEEvent
	if err := json.Unmarshal(payload, &event); err != nil {
		debugLog("[SSE] Failed to parse payload: %v", err)
		return
	}

	debugLog("[SSE] Event: %s", event.Type)

	c.sendEvent(&event)
}

func (c *TunnelClient) sendEvent(event *SSEEvent) {
	eventJSON, err := json.Marshal(event)
	if err != nil {
		debugLog("[SSE] Failed to marshal event: %v", err)
		return
	}

	var tunnelEvent TunnelEvent
	tunnelEvent.Event = "sse"

	if c.config.EncryptionKey != "" {
		encrypted, err := encrypt(eventJSON, c.config.EncryptionKey)
		if err != nil {
			debugLog("[SSE] Failed to encrypt event: %v", err)
			return
		}
		tunnelEvent.Data = string(encrypted)
	} else {
		tunnelEvent.Data = string(eventJSON)
	}

	data, _ := json.Marshal(tunnelEvent)
	c.writeMu.Lock()
	err = c.conn.WriteMessage(websocket.TextMessage, data)
	c.writeMu.Unlock()

	if err != nil {
		debugLog("[SSE] Failed to send event: %v", err)
	}
}

func (c *TunnelClient) handleRequest(req *TunnelRequest) {
	debugLog("[debug] Received request: %s %s", req.Method, req.Path)
	if strings.Contains(req.Path, "prompt_async") && len(req.Body) > 0 {
		debugLog("[debug] Request body (first 500 chars): %s", string(req.Body[:min(len(req.Body), 500)]))
	}

	if req.Path == "/discover" || req.Path == "/discover/" {
		c.handleDiscover(req)
		return
	}

	if strings.HasPrefix(req.Path, "/lazy-image/") {
		c.handleLazyImage(req)
		return
	}

	if c.isStreamMode() && requestAcceptsEventStream(req) {
		c.handleRequestStream(req)
		return
	}
	c.handleRequestLegacy(req)
}

// requestAcceptsEventStream reports whether the forwarded request asked for
// SSE. The relay only relays text/event-stream requests incrementally; every
// other request still uses the single legacy response frame, even on a v2
// tunnel.
func requestAcceptsEventStream(req *TunnelRequest) bool {
	for key, value := range req.Headers {
		if !strings.EqualFold(key, "Accept") {
			continue
		}
		for _, part := range strings.Split(value, ",") {
			mediaType := strings.TrimSpace(strings.SplitN(part, ";", 2)[0])
			if strings.EqualFold(mediaType, "text/event-stream") {
				return true
			}
		}
	}
	return false
}

// applyLocalAuth attaches the local OpenCode server's Basic credentials when
// the tunnel config provides them. The relay strips its own Authorization
// header, so this is the only place local credentials are added.
func (c *TunnelClient) applyLocalAuth(h http.Header) {
	if c == nil || c.localAuthUser == "" {
		return
	}
	h.Set("Authorization", "Basic "+base64.StdEncoding.EncodeToString([]byte(c.localAuthUser+":"+c.localAuthPassword)))
}

func (c *TunnelClient) setStreamMode(enabled bool) {
	c.modeMu.Lock()
	c.streamMode = enabled
	c.modeMu.Unlock()
}

func (c *TunnelClient) isStreamMode() bool {
	c.modeMu.RLock()
	defer c.modeMu.RUnlock()
	return c.streamMode
}

// newLocalRequest snapshots a tunnel request into a request against the local
// OpenCode server. On failure it returns the status code and message to report.
func (c *TunnelClient) newLocalRequest(req *TunnelRequest) (*http.Request, int, string) {
	localURL := fmt.Sprintf("http://127.0.0.1:%s%s", c.localPort, req.Path)

	requestBody := req.Body
	if c.config.EncryptionKey != "" && len(req.Body) > 0 {
		decrypted, err := decrypt(req.Body, c.config.EncryptionKey)
		if err != nil {
			return nil, 400, "failed to decrypt request"
		}
		requestBody = decrypted
	}

	httpReq, err := http.NewRequest(req.Method, localURL, bytes.NewReader(requestBody))
	if err == nil {
		c.applyLocalAuth(httpReq.Header)
	}
	if err != nil {
		return nil, 500, "failed to create request"
	}

	for key, value := range req.Headers {
		httpReq.Header.Set(key, value)
	}
	// The tunnel re-frames bodies itself (and encrypts them when E2E is on), so
	// transport compression must never be negotiated: a compressed body would
	// be forwarded with a stale Content-Encoding or fail to decrypt.
	httpReq.Header.Set("Accept-Encoding", "identity")
	return httpReq, 0, ""
}

// handleRequestLegacy buffers the whole local response into a single
// TunnelResponse frame. It is used with relays that do not speak protocol v2.
func (c *TunnelClient) handleRequestLegacy(req *TunnelRequest) {
	httpReq, status, message := c.newLocalRequest(req)
	if httpReq == nil {
		c.sendErrorResponse(req.ID, status, message)
		return
	}

	resp, err := c.httpClient.Do(httpReq)
	if err != nil {
		c.sendErrorResponse(req.ID, 502, "local service unavailable")
		return
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		c.sendErrorResponse(req.ID, 502, "failed to read response")
		return
	}

	originalSize := len(body)
	start := time.Now()
	if strippedBody, stripped := stripBase64Images(body, req.Path); stripped {
		body = strippedBody
		debugLog("[debug] Stripped images in %dms, size reduced from %d to %d bytes",
			time.Since(start).Milliseconds(), originalSize, len(body))
	}

	responseBody := body
	if c.config.EncryptionKey != "" && len(body) > 0 {
		encrypted, err := encrypt(body, c.config.EncryptionKey)
		if err != nil {
			c.sendErrorResponse(req.ID, 500, "failed to encrypt response")
			return
		}
		responseBody = encrypted
	}

	headers := make(map[string]string)
	for key := range resp.Header {
		headers[key] = resp.Header.Get(key)
	}

	headers["Content-Length"] = fmt.Sprintf("%d", len(responseBody))

	// If encrypted, override Content-Type to text/plain so clients don't try to parse as JSON
	if c.config.EncryptionKey != "" {
		headers["Content-Type"] = "text/plain"
		// The body is now ciphertext: any transport encoding the local server
		// negotiated no longer describes it, and forwarding it makes HTTP
		// clients try to decompress random bytes.
		deleteHeaderFold(headers, "Content-Encoding")
		deleteHeaderFold(headers, "Transfer-Encoding")
	}

	tunnelResp := &TunnelResponse{
		ID:         req.ID,
		StatusCode: resp.StatusCode,
		Headers:    headers,
		Body:       responseBody,
	}

	data, _ := json.Marshal(tunnelResp)
	c.writeMu.Lock()
	err = c.conn.WriteMessage(websocket.TextMessage, data)
	c.writeMu.Unlock()
	if err != nil {
		debugLog("[debug] Failed to send response: %v", err)
	} else {
		debugLog("[debug] Sent response: status=%d, bodyLen=%d", resp.StatusCode, len(responseBody))
	}
}

// handleRequestStream forwards the local response as res-head, res-chunk...,
// res-end without buffering the body. Each request gets its own context so a
// req-cancel or a dropped tunnel stops the local handler promptly.
func (c *TunnelClient) handleRequestStream(req *TunnelRequest) {
	httpReq, _, message := c.newLocalRequest(req)
	if httpReq == nil {
		c.sendFrame(&tunnel.ResError{ID: req.ID, Message: message})
		return
	}

	ctx, cancel := context.WithCancel(context.Background())
	handle := c.registerStreamCancel(req.ID, cancel)
	defer func() {
		c.unregisterStreamCancel(req.ID, handle)
		cancel()
	}()

	resp, err := c.httpClient.Do(httpReq.WithContext(ctx))
	if err != nil {
		if ctx.Err() == nil {
			c.sendFrame(&tunnel.ResError{ID: req.ID, Message: "local service unavailable: " + err.Error()})
		}
		return
	}
	defer resp.Body.Close()

	// Streaming frames each carry their own payload: when an E2E key exists the
	// payloads are encrypted per chunk and the head marks that fact. The relay
	// only moves bytes and never needs the key (design D8). Content-Length is
	// dropped because the relay writes the body incrementally.
	headers := make(map[string]string, len(resp.Header))
	encryptedStream := c.streamEncryptionKey() != ""
	for key := range resp.Header {
		if isHopByHopHeader(key) || strings.EqualFold(key, "Content-Length") {
			continue
		}
		if encryptedStream && strings.EqualFold(key, "Content-Encoding") {
			continue
		}
		headers[key] = resp.Header.Get(key)
	}

	if err := c.sendResHead(req.ID, resp.StatusCode, headers); err != nil {
		debugLog("[stream] failed to send res-head for %s: %v", req.ID, err)
		return
	}

	keyHex := c.streamEncryptionKey()
	buf := make([]byte, streamChunkSize)
	var seq int64
	for {
		n, readErr := resp.Body.Read(buf)
		if n > 0 {
			if ctx.Err() != nil {
				return
			}
			// The relay keeps the chunk until it is written, so hand it a copy
			// instead of reusing buf.
			data := make([]byte, n)
			copy(data, buf[:n])
			if keyHex != "" {
				encrypted, err := encryptSSEChunk(data, keyHex)
				if err != nil {
					log.Printf("[stream] failed to encrypt chunk %d for %s: %v", seq, req.ID, err)
					c.sendFrame(&tunnel.ResError{ID: req.ID, Message: "failed to encrypt response"})
					return
				}
				data = encrypted
			}
			if err := c.sendFrame(&tunnel.ResChunk{ID: req.ID, Seq: seq, Data: data}); err != nil {
				debugLog("[stream] failed to send chunk %d for %s: %v", seq, req.ID, err)
				return
			}
			seq++
		}

		if readErr != nil {
			switch {
			case readErr == io.EOF:
				if err := c.sendFrame(&tunnel.ResEnd{ID: req.ID}); err != nil {
					debugLog("[stream] failed to send res-end for %s: %v", req.ID, err)
				}
			case ctx.Err() != nil:
				// Cancelled by the relay or by connection teardown: the stream
				// is already gone, so do not report an error.
			default:
				c.sendFrame(&tunnel.ResError{ID: req.ID, Message: "failed to read response: " + readErr.Error()})
			}
			return
		}

		if ctx.Err() != nil {
			return
		}
	}
}

// sendResHead encodes and writes a res-head, trimming headers if the encoded
// frame would exceed MaxFrameBytes. Dropping is logged, never silent. It always
// records whether stream payloads are encrypted so the mobile client does not
// have to guess.
func (c *TunnelClient) sendResHead(id string, statusCode int, headers map[string]string) error {
	if headers == nil {
		headers = make(map[string]string)
	}
	if c.streamEncryptionEnabled() {
		headers[streamE2EHeader] = "1"
	} else {
		headers[streamE2EHeader] = "0"
	}
	head := &tunnel.ResHead{ID: id, StatusCode: statusCode, Headers: headers}
	data, err := tunnel.EncodeFrame(head)
	if err == nil {
		return c.writeFrameBytes(data)
	}
	if !errors.Is(err, tunnel.ErrFrameTooLarge) {
		return err
	}

	keys := make([]string, 0, len(headers))
	for key := range headers {
		keys = append(keys, key)
	}
	// Drop the largest values first: one huge header should not cost the rest
	// of the response metadata.
	sort.Slice(keys, func(i, j int) bool {
		if len(headers[keys[i]]) != len(headers[keys[j]]) {
			return len(headers[keys[i]]) > len(headers[keys[j]])
		}
		return keys[i] < keys[j]
	})

	trimmed := make(map[string]string, len(headers))
	for key, value := range headers {
		trimmed[key] = value
	}
	dropped := 0
	for len(keys) > 0 {
		largest := keys[0]
		keys = keys[1:]
		delete(trimmed, largest)
		dropped++

		head.Headers = trimmed
		data, err = tunnel.EncodeFrame(head)
		if err == nil {
			log.Printf("[stream] res-head for %s exceeded MaxFrameBytes; dropped %d header(s)", id, dropped)
			return c.writeFrameBytes(data)
		}
		if !errors.Is(err, tunnel.ErrFrameTooLarge) {
			return err
		}
	}

	head.Headers = nil
	data, err = tunnel.EncodeFrame(head)
	if err != nil {
		return err
	}
	log.Printf("[stream] res-head for %s exceeded MaxFrameBytes even without headers; dropped all %d header(s)", id, dropped)
	return c.writeFrameBytes(data)
}

// sendFrame encodes a typed protocol frame and writes it to the tunnel.
func (c *TunnelClient) sendFrame(frame any) error {
	data, err := tunnel.EncodeFrame(frame)
	if err != nil {
		return err
	}
	return c.writeFrameBytes(data)
}

func (c *TunnelClient) writeFrameBytes(data []byte) error {
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	if c.conn == nil {
		return errors.New("tunnel not connected")
	}
	c.conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
	return c.conn.WriteMessage(websocket.TextMessage, data)
}

func (c *TunnelClient) registerStreamCancel(id string, cancel context.CancelFunc) *streamCancel {
	handle := &streamCancel{cancel: cancel}

	c.streamsMu.Lock()
	if c.streamCancels == nil {
		c.streamCancels = make(map[string]*streamCancel)
	}
	prev := c.streamCancels[id]
	c.streamCancels[id] = handle
	c.streamsMu.Unlock()

	if prev != nil {
		prev.cancel()
	}
	return handle
}

func (c *TunnelClient) unregisterStreamCancel(id string, handle *streamCancel) {
	c.streamsMu.Lock()
	if c.streamCancels[id] == handle {
		delete(c.streamCancels, id)
	}
	c.streamsMu.Unlock()
}

func (c *TunnelClient) handleReqCancel(frame *tunnel.ReqCancel) {
	c.streamsMu.Lock()
	handle := c.streamCancels[frame.ID]
	delete(c.streamCancels, frame.ID)
	c.streamsMu.Unlock()

	if handle != nil {
		handle.cancel()
	}
	// The same ID may also name a local WebSocket. Cancelling an unknown ID is
	// a no-op, so repeated cancels are safe.
	c.closeWsSession(frame.ID, websocket.CloseNormalClosure, "request cancelled", false)
}

func (c *TunnelClient) cancelAllStreams() {
	c.streamsMu.Lock()
	handles := c.streamCancels
	c.streamCancels = make(map[string]*streamCancel)
	c.streamsMu.Unlock()

	for _, handle := range handles {
		handle.cancel()
	}
}

// ---------------------------------------------------------------------------
// Local WebSocket bridging (design D4/D5).
// ---------------------------------------------------------------------------

// isHopByHopHeader reports connection-scoped headers that must not be copied to
// the local WebSocket handshake; the dialer generates its own.
// deleteHeaderFold removes a header regardless of the casing the upstream used.
func deleteHeaderFold(headers map[string]string, name string) {
	for key := range headers {
		if strings.EqualFold(key, name) {
			delete(headers, key)
		}
	}
}

func isHopByHopHeader(header string) bool {
	switch strings.ToLower(header) {
	case "connection", "keep-alive", "proxy-authenticate", "proxy-authorization",
		"te", "trailer", "transfer-encoding", "upgrade":
		return true
	}
	return false
}

// isBlockedLocalHeader reports relay credentials and handshake headers that
// must not reach the local endpoint. This mirrors the relay-side rule in
// internal/tunnel/handler.go.
func isBlockedLocalHeader(header string) bool {
	lower := strings.ToLower(header)
	switch lower {
	case "authorization", "cookie", "x-real-ip", "origin",
		"sec-websocket-key", "sec-websocket-version", "sec-websocket-extensions",
		"sec-websocket-protocol", "sec-websocket-accept", "host":
		return true
	}
	return strings.HasPrefix(lower, "x-forwarded-")
}

// handleWsOpen dials the local endpoint and bridges it to the relay. The
// session is reserved before dialing so a duplicate ws-open is rejected even
// while the first dial is still in flight.
func (c *TunnelClient) handleWsOpen(open *tunnel.WsOpen) {
	if open == nil || open.ID == "" {
		return
	}
	encrypted := c.streamEncryptionEnabled()

	c.wsMu.Lock()
	if c.wsConns == nil {
		c.wsConns = make(map[string]*wsSession)
	}
	if _, exists := c.wsConns[open.ID]; exists {
		c.wsMu.Unlock()
		c.sendFrame(&tunnel.WsAck{
			ID:        open.ID,
			Status:    http.StatusConflict,
			Error:     "websocket already open",
			Encrypted: encrypted,
		})
		return
	}
	sess := &wsSession{encrypted: encrypted}
	c.wsConns[open.ID] = sess
	c.wsMu.Unlock()

	path := open.Path
	if path == "" {
		path = "/"
	}
	if !strings.HasPrefix(path, "/") {
		path = "/" + path
	}
	target := fmt.Sprintf("ws://127.0.0.1:%s%s", c.localPort, path)

	header := http.Header{}
	for key, value := range open.Headers {
		if isHopByHopHeader(key) || isBlockedLocalHeader(key) {
			continue
		}
		header.Set(key, value)
	}
	c.applyLocalAuth(header)

	dialer := websocket.Dialer{
		HandshakeTimeout:  10 * time.Second,
		EnableCompression: false,
		Subprotocols:      open.Subprotocols,
	}
	conn, resp, err := dialer.Dial(target, header)
	if err != nil {
		if resp != nil && resp.Body != nil {
			body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
			resp.Body.Close()
			safePath := path
			if i := strings.IndexByte(safePath, '?'); i >= 0 {
				safePath = safePath[:i]
			}
			debugLog("[ws] dial %s status=%d body=%q", safePath, resp.StatusCode, string(body))
		}
		c.dropWsSession(open.ID, sess)
		message := err.Error()
		if resp != nil {
			message = fmt.Sprintf("local websocket returned status %d", resp.StatusCode)
		}
		debugLog("[ws] dial %s failed: %v", path, err)
		c.sendFrame(&tunnel.WsAck{
			ID:        open.ID,
			Status:    http.StatusBadGateway,
			Error:     message,
			Encrypted: encrypted,
		})
		return
	}

	c.wsMu.Lock()
	if c.wsConns[open.ID] != sess {
		// The tunnel went away while dialing.
		c.wsMu.Unlock()
		conn.Close()
		return
	}
	sess.conn = conn
	c.wsMu.Unlock()

	if err := c.sendFrame(&tunnel.WsAck{
		ID:          open.ID,
		OK:          true,
		Status:      http.StatusSwitchingProtocols,
		Subprotocol: conn.Subprotocol(),
		Encrypted:   encrypted,
	}); err != nil {
		c.closeWsSession(open.ID, websocket.CloseGoingAway, "tunnel write failed", false)
		return
	}

	go c.pumpLocalWs(open.ID, sess)
}

// pumpLocalWs forwards local frames to the relay until the socket closes. When
// the session is encrypted every data frame is sealed on its own; a sealing
// failure ends the session instead of leaking plaintext.
func (c *TunnelClient) pumpLocalWs(id string, sess *wsSession) {
	for {
		opcode, data, err := sess.conn.ReadMessage()
		if err != nil {
			code := websocket.CloseInternalServerErr
			reason := err.Error()
			var closeErr *websocket.CloseError
			if errors.As(err, &closeErr) {
				code = closeErr.Code
				reason = closeErr.Text
				if code == websocket.CloseAbnormalClosure || code == websocket.CloseNoStatusReceived {
					code = websocket.CloseInternalServerErr
				}
			}
			c.closeWsSession(id, code, reason, true)
			return
		}

		if sess.encrypted {
			// Ciphertext must travel as a binary frame: a text frame would be
			// UTF-8 decoded on the app side and the bytes would be lost. The
			// original opcode is sealed inside the plaintext.
			plain := make([]byte, len(data)+1)
			plain[0] = byte(opcode)
			copy(plain[1:], data)
			encrypted, encErr := encryptStreamPayload(plain, c.streamEncryptionKey())
			if encErr != nil {
				log.Printf("[ws] failed to encrypt frame for %s: %v", id, encErr)
				c.closeWsSession(id, websocket.CloseInternalServerErr, "failed to encrypt frame", true)
				return
			}
			data = encrypted
			opcode = websocket.BinaryMessage
		}

		if err := c.sendFrame(&tunnel.WsFrame{ID: id, Opcode: opcode, Data: data}); err != nil {
			debugLog("[ws] failed to forward frame for %s: %v", id, err)
			c.closeWsSession(id, websocket.CloseInternalServerErr, "tunnel write failed", false)
			return
		}
	}
}

// handleWsFrame writes one relay frame to the matching local socket. Encrypted
// sessions must decrypt successfully: a failure closes the socket with 1011 and
// never writes ciphertext to the local endpoint.
func (c *TunnelClient) handleWsFrame(frame *tunnel.WsFrame) {
	if frame == nil {
		return
	}
	c.wsMu.Lock()
	sess := c.wsConns[frame.ID]
	c.wsMu.Unlock()
	if sess == nil {
		debugLog("[ws] dropping frame for unknown session %s", frame.ID)
		return
	}
	if frame.Opcode != websocket.TextMessage && frame.Opcode != websocket.BinaryMessage {
		debugLog("[ws] ignoring control opcode %d for %s", frame.Opcode, frame.ID)
		return
	}

	data := frame.Data
	opcode := frame.Opcode
	if sess.encrypted {
		plain, err := decryptStreamPayload(frame.Data, c.streamEncryptionKey())
		if err != nil {
			log.Printf("[ws] failed to decrypt frame for %s: %v", frame.ID, err)
			c.closeWsSession(frame.ID, websocket.CloseInternalServerErr, "failed to decrypt frame", true)
			return
		}
		if len(plain) < 1 {
			c.closeWsSession(frame.ID, websocket.CloseInternalServerErr, "empty encrypted frame", true)
			return
		}
		opcode = int(plain[0])
		data = plain[1:]
		if opcode != websocket.TextMessage && opcode != websocket.BinaryMessage {
			c.closeWsSession(frame.ID, websocket.CloseInternalServerErr, "invalid sealed opcode", true)
			return
		}
	}

	sess.writeMu.Lock()
	var err error
	if sess.conn != nil {
		sess.conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
		err = sess.conn.WriteMessage(opcode, data)
	}
	sess.writeMu.Unlock()

	if err != nil {
		debugLog("[ws] failed to write to local socket %s: %v", frame.ID, err)
		c.closeWsSession(frame.ID, websocket.CloseInternalServerErr, "local write failed", true)
	}
}

// handleWsClose closes the local socket with the relay's code. The local read
// loop then sees the close and does not echo a duplicate ws-close back.
func (c *TunnelClient) handleWsClose(frame *tunnel.WsClose) {
	if frame == nil {
		return
	}
	code := frame.Code
	if code == 0 {
		code = websocket.CloseNormalClosure
	}
	// 1005/1006/1015 are local-only statuses and cannot be put on the wire.
	switch code {
	case websocket.CloseNoStatusReceived, websocket.CloseAbnormalClosure, websocket.CloseTLSHandshake:
		code = websocket.CloseInternalServerErr
	}
	c.closeWsSession(frame.ID, code, frame.Reason, false)
}

// closeWsSession removes the session and closes the local socket exactly once.
// notifyRelay reports the close upstream for locally-initiated closes.
func (c *TunnelClient) closeWsSession(id string, code int, reason string, notifyRelay bool) {
	c.wsMu.Lock()
	sess := c.wsConns[id]
	if sess != nil {
		delete(c.wsConns, id)
	}
	c.wsMu.Unlock()
	if sess == nil {
		return
	}

	sess.once.Do(func() {
		sess.writeMu.Lock()
		if sess.conn != nil {
			sess.conn.WriteControl(
				websocket.CloseMessage,
				websocket.FormatCloseMessage(code, truncateCloseReason(reason)),
				time.Now().Add(time.Second),
			)
			sess.conn.Close()
		}
		sess.writeMu.Unlock()

		if notifyRelay {
			c.sendFrame(&tunnel.WsClose{ID: id, Code: code, Reason: truncateCloseReason(reason)})
		}
	})
}

// dropWsSession removes a reservation whose dial failed, without touching a
// connection that another path may have installed.
func (c *TunnelClient) dropWsSession(id string, sess *wsSession) {
	c.wsMu.Lock()
	if c.wsConns[id] == sess {
		delete(c.wsConns, id)
	}
	c.wsMu.Unlock()
}

// closeAllWs closes every local socket, e.g. when the tunnel disconnects.
func (c *TunnelClient) closeAllWs() {
	c.wsMu.Lock()
	ids := make([]string, 0, len(c.wsConns))
	for id := range c.wsConns {
		ids = append(ids, id)
	}
	c.wsMu.Unlock()

	for _, id := range ids {
		c.closeWsSession(id, websocket.CloseGoingAway, "tunnel disconnected", false)
	}
}

// truncateCloseReason keeps a close reason inside the 123-byte protocol limit.
func truncateCloseReason(reason string) string {
	const maxCloseReason = 123
	if len(reason) <= maxCloseReason {
		return reason
	}
	cut := maxCloseReason
	for cut > 0 && !utf8.RuneStart(reason[cut]) {
		cut--
	}
	return reason[:cut]
}

func (c *TunnelClient) sendErrorResponse(reqID string, statusCode int, message string) {
	body := []byte(message)
	if c.config.EncryptionKey != "" {
		encrypted, err := encrypt(body, c.config.EncryptionKey)
		if err == nil {
			body = encrypted
		}
	}

	resp := &TunnelResponse{
		ID:         reqID,
		StatusCode: statusCode,
		Headers: map[string]string{
			"Content-Type":   "text/plain",
			"Content-Length": fmt.Sprintf("%d", len(body)),
		},
		Body: body,
	}

	data, _ := json.Marshal(resp)
	c.writeMu.Lock()
	c.conn.WriteMessage(websocket.TextMessage, data)
	c.writeMu.Unlock()
}

func (c *TunnelClient) handleLazyImage(req *TunnelRequest) {
	partID := strings.TrimPrefix(req.Path, "/lazy-image/")

	url, ok := imageCache.Load(partID)
	if !ok {
		debugLog("[debug] Image not found in cache for partID: %s", partID)
		c.sendErrorResponse(req.ID, 404, `{"error": "not found"}`)
		return
	}

	debugLog("[debug] Serving cached image for partID: %s", partID)

	responseJSON, _ := json.Marshal(map[string]string{"url": url.(string)})

	responseBody := responseJSON
	if c.config.EncryptionKey != "" {
		encrypted, err := encrypt(responseJSON, c.config.EncryptionKey)
		if err != nil {
			c.sendErrorResponse(req.ID, 500, "failed to encrypt response")
			return
		}
		responseBody = encrypted
	}

	contentType := "application/json"
	if c.config.EncryptionKey != "" {
		contentType = "text/plain"
	}

	tunnelResp := &TunnelResponse{
		ID:         req.ID,
		StatusCode: 200,
		Headers: map[string]string{
			"Content-Type":   contentType,
			"Content-Length": fmt.Sprintf("%d", len(responseBody)),
		},
		Body: responseBody,
	}

	data, _ := json.Marshal(tunnelResp)
	c.writeMu.Lock()
	err := c.conn.WriteMessage(websocket.TextMessage, data)
	c.writeMu.Unlock()
	if err != nil {
		debugLog("[debug] Failed to send lazy-image response: %v", err)
	}
}

func (c *TunnelClient) handleDiscover(req *TunnelRequest) {
	localURL := fmt.Sprintf("http://127.0.0.1:%s/project", c.localPort)

	httpReq, err := http.NewRequest("GET", localURL, nil)
	if err != nil {
		c.sendErrorResponse(req.ID, 500, "failed to create request")
		return
	}

	resp, err := c.httpClient.Do(httpReq)
	if err != nil {
		c.sendErrorResponse(req.ID, 502, "opencode unavailable")
		return
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		c.sendErrorResponse(req.ID, 502, "failed to read response")
		return
	}

	responseBody := body
	if c.config.EncryptionKey != "" {
		encrypted, err := encrypt(body, c.config.EncryptionKey)
		if err != nil {
			c.sendErrorResponse(req.ID, 500, "failed to encrypt response")
			return
		}
		responseBody = encrypted
	}

	contentType := "application/json"
	if c.config.EncryptionKey != "" {
		contentType = "text/plain"
	}

	tunnelResp := &TunnelResponse{
		ID:         req.ID,
		StatusCode: resp.StatusCode,
		Headers: map[string]string{
			"Content-Type":   contentType,
			"Content-Length": fmt.Sprintf("%d", len(responseBody)),
		},
		Body: responseBody,
	}

	data, _ := json.Marshal(tunnelResp)
	c.writeMu.Lock()
	err = c.conn.WriteMessage(websocket.TextMessage, data)
	c.writeMu.Unlock()
	if err != nil {
		debugLog("[debug] Failed to send discover response: %v", err)
	}
}

// stripBase64Images strips base64 image data from message list responses
// and replaces them with lazy:{partID} placeholders for on-demand loading.
func stripBase64Images(body []byte, path string) ([]byte, bool) {
	// Only process message list endpoint (with or without query params)
	matched, _ := regexp.MatchString(`^/session/[^/]+/message(\?.*)?$`, path)
	if !matched {
		return body, false
	}

	// Parse JSON as array of messages
	var messages []map[string]interface{}
	if err := json.Unmarshal(body, &messages); err != nil {
		return body, false // Not valid JSON array, return unchanged
	}

	modified := false
	for _, msg := range messages {
		parts, ok := msg["parts"].([]interface{})
		if !ok {
			continue
		}
		for _, p := range parts {
			part, ok := p.(map[string]interface{})
			if !ok {
				continue
			}
			if part["type"] == "file" {
				url, _ := part["url"].(string)
				if strings.HasPrefix(url, "data:image/") {
					partID, _ := part["id"].(string)
					imageCache.Store(partID, url)
					debugLog("[debug] Cached image for partID: %s (size: %d)", partID, len(url))
					part["url"] = "lazy:" + partID
					modified = true
				}
			}
		}
	}

	if !modified {
		return body, false
	}

	newBody, err := json.Marshal(messages)
	if err != nil {
		return body, false
	}
	return newBody, true
}

// configDirOverride lets tests point config reads/writes at t.TempDir().
var configDirOverride string

func getConfigDir() string {
	if configDirOverride != "" {
		return configDirOverride
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, configDir)
}

func loadAuthConfig() (*AuthConfig, error) {
	path := filepath.Join(getConfigDir(), authFileName)
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var config AuthConfig
	if err := json.Unmarshal(data, &config); err != nil {
		return nil, err
	}
	return &config, nil
}

func saveAuthConfig(config *AuthConfig) error {
	dir := getConfigDir()
	if err := os.MkdirAll(dir, 0700); err != nil {
		return err
	}
	data, _ := json.MarshalIndent(config, "", "  ")
	return os.WriteFile(filepath.Join(dir, authFileName), data, 0600)
}

func loadDeviceConfig() (*DeviceConfig, error) {
	path := filepath.Join(getConfigDir(), deviceFileName)
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var config DeviceConfig
	if err := json.Unmarshal(data, &config); err != nil {
		return nil, err
	}
	if config.Subdomain == "" || config.AuthUser == "" || config.AuthPassword == "" {
		return nil, fmt.Errorf("incomplete device config")
	}
	if config.HostID == "" {
		config.HostID = config.Subdomain
	}
	if config.HostName == "" {
		config.HostName, _ = os.Hostname()
	}
	return &config, nil
}

func saveDeviceConfig(config *DeviceConfig) error {
	dir := getConfigDir()
	if err := os.MkdirAll(dir, 0700); err != nil {
		return err
	}
	data, _ := json.MarshalIndent(config, "", "  ")
	return os.WriteFile(filepath.Join(dir, deviceFileName), data, 0600)
}

// setupAutoStart is a package-level var so tests can observe the install call.
var setupAutoStart = func(localPort string) error {
	switch runtime.GOOS {
	case "darwin":
		return setupLaunchd(localPort)
	case "linux":
		return setupSystemd(localPort)
	case "windows":
		return setupWindowsTask(localPort)
	default:
		return fmt.Errorf("unsupported OS: %s", runtime.GOOS)
	}
}

func setupLaunchd(localPort string) error {
	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}

	execPath, err := os.Executable()
	if err != nil {
		return err
	}

	launchAgentsDir := filepath.Join(home, "Library", "LaunchAgents")
	if err := os.MkdirAll(launchAgentsDir, 0755); err != nil {
		return err
	}

	logDir := filepath.Join(home, ".opencode-relay")
	if err := os.MkdirAll(logDir, 0755); err != nil {
		return err
	}

	plistPath := filepath.Join(launchAgentsDir, "com.opencode.relay.plist")
	logPath := filepath.Join(logDir, "tunnel.log")

	plistContent := fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
    <key>Label</key>
    <string>com.opencode.relay</string>
    <key>ProgramArguments</key>
    <array>
        <string>%s</string>
        <string>start</string>
        <string>-port</string>
        <string>%s</string>
    </array>
    <key>RunAtLoad</key>
    <true/>
    <key>KeepAlive</key>
    <true/>
    <key>StandardOutPath</key>
    <string>%s</string>
    <key>StandardErrorPath</key>
    <string>%s</string>
</dict>
</plist>
`, execPath, localPort, logPath, logPath)

	if err := os.WriteFile(plistPath, []byte(plistContent), 0644); err != nil {
		return err
	}

	exec.Command("launchctl", "unload", plistPath).Run()

	cmd := exec.Command("launchctl", "load", plistPath)
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("failed to load launchd plist: %w", err)
	}

	return nil
}

func setupSystemd(localPort string) error {
	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}

	execPath, err := os.Executable()
	if err != nil {
		return err
	}

	systemdDir := filepath.Join(home, ".config", "systemd", "user")
	if err := os.MkdirAll(systemdDir, 0755); err != nil {
		return err
	}

	servicePath := filepath.Join(systemdDir, "opencode-relay.service")

	serviceContent := fmt.Sprintf(`[Unit]
Description=OpenCode Anywhere Tunnel
After=network.target

[Service]
Type=simple
ExecStart=%s start -port %s
Restart=always
RestartSec=5

[Install]
WantedBy=default.target
`, execPath, localPort)

	if err := os.WriteFile(servicePath, []byte(serviceContent), 0644); err != nil {
		return err
	}

	exec.Command("systemctl", "--user", "daemon-reload").Run()

	cmd := exec.Command("systemctl", "--user", "enable", "opencode-relay.service")
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("failed to enable systemd service: %w", err)
	}

	return nil
}

func setupWindowsTask(localPort string) error {
	execPath, err := os.Executable()
	if err != nil {
		return err
	}

	taskName := "OpenCodeRelay"

	exec.Command("schtasks", "/Delete", "/TN", taskName, "/F").Run()

	cmd := exec.Command("schtasks", "/Create",
		"/TN", taskName,
		"/TR", fmt.Sprintf(`"%s" start -port %s`, execPath, localPort),
		"/SC", "ONLOGON",
		"/RL", "HIGHEST",
		"/F")

	if err := cmd.Run(); err != nil {
		return fmt.Errorf("failed to create scheduled task: %w", err)
	}

	return nil
}

var errNoPassphrase = errors.New("no E2E passphrase provided")

// stdinIsTerminal is a seam so tests never block on a prompt.
var stdinIsTerminal = func() bool { return term.IsTerminal(int(os.Stdin.Fd())) }

// resolvePassphrase applies D5 priority: --passphrase > env > interactive prompt.
func resolvePassphrase(flagValue string) (string, error) {
	if flagValue != "" {
		return flagValue, nil
	}
	if p := os.Getenv("OPENCODE_TUNNEL_PASSPHRASE"); p != "" {
		return p, nil
	}
	if !stdinIsTerminal() {
		return "", errNoPassphrase
	}
	fmt.Print("E2E 口令（不会保存）: ")
	raw, err := term.ReadPassword(int(os.Stdin.Fd()))
	fmt.Println()
	if err != nil {
		return "", fmt.Errorf("读取口令失败: %w", err)
	}
	if len(raw) == 0 {
		return "", errNoPassphrase
	}
	return string(raw), nil
}

// ensureHostKey wraps the host key when key material is missing or --rewrap is
// set. It only reports errNoPassphrase when wrapping is actually needed.
func ensureHostKey(device *DeviceConfig, passphrase string, rewrap bool) (bool, error) {
	if !rewrap && device.KeyWrapped != "" && device.KeySalt != "" && device.KeyKdf != "" {
		return false, nil
	}
	if passphrase == "" {
		return false, errNoPassphrase
	}
	if device.EncryptionKey == "" {
		key, err := generateEncryptionKey()
		if err != nil {
			return false, err
		}
		device.EncryptionKey = key
	}

	wrapped, salt, err := wrapHostKey(passphrase, device.EncryptionKey)
	if err != nil {
		return false, err
	}
	device.KeyWrapped, device.KeySalt, device.KeyKdf = wrapped, salt, keyKdfScrypt
	return true, nil
}

// wrapHostKey encrypts the hex host key with a passphrase-derived KEK (D5).
func wrapHostKey(passphrase, hostKeyHex string) (wrapped, saltB64 string, err error) {
	hostKey, err := hex.DecodeString(hostKeyHex)
	if err != nil {
		return "", "", err
	}

	salt := make([]byte, 16)
	if _, err := rand.Read(salt); err != nil {
		return "", "", err
	}
	kek, err := scrypt.Key([]byte(passphrase), salt, 32768, 8, 1, 32)
	if err != nil {
		return "", "", err
	}

	wrappedBytes, err := encrypt(hostKey, hex.EncodeToString(kek))
	if err != nil {
		return "", "", err
	}
	return string(wrappedBytes), base64.StdEncoding.EncodeToString(salt), nil
}

func unwrapHostKey(passphrase, wrapped, saltB64 string) (string, error) {
	salt, err := base64.StdEncoding.DecodeString(saltB64)
	if err != nil {
		return "", fmt.Errorf("密钥盐无效: %w", err)
	}
	kek, err := scrypt.Key([]byte(passphrase), salt, 32768, 8, 1, 32)
	if err != nil {
		return "", err
	}

	hostKey, err := decrypt([]byte(wrapped), hex.EncodeToString(kek))
	if err != nil {
		return "", fmt.Errorf("解包失败：口令错误或密钥材料损坏")
	}
	return hex.EncodeToString(hostKey), nil
}

// prepareHostKey persists wrapped key material and reports it. Reporting never
// blocks the tunnel: failures are warnings retried on the next start. It returns
// errNoPassphrase when wrapping was needed but no passphrase was available.
func prepareHostKey(relay, token string, device *DeviceConfig, passphrase string, rewrap bool) error {
	changed, err := ensureHostKey(device, passphrase, rewrap)
	if errors.Is(err, errNoPassphrase) {
		passphrase, err = resolvePassphrase("")
		if err != nil {
			return errNoPassphrase
		}
		changed, err = ensureHostKey(device, passphrase, rewrap)
	}
	if err != nil {
		fmt.Printf("  ⚠ E2E 密钥包裹失败: %v\n", err)
		return err
	}

	if changed {
		if err := saveDeviceConfig(device); err != nil {
			fmt.Printf("  ⚠ 保存 device.json 失败: %v\n", err)
			return err
		}
	}

	if err := reportHostKey(relay, token, device.HostID, device); err != nil {
		fmt.Printf("  ⚠ E2E 密钥上报失败: %v（隧道继续运行，下次启动重试）\n", err)
	}
	return nil
}

// reportHostKey retries transient failures up to keyReportAttempts; credential
// failures stop immediately.
func reportHostKey(relay, token, hostID string, device *DeviceConfig) error {
	body, _ := json.Marshal(map[string]string{
		"key_wrapped": device.KeyWrapped,
		"key_salt":    device.KeySalt,
		"key_kdf":     device.KeyKdf,
	})

	var lastErr error
	for attempt := 1; attempt <= keyReportAttempts; attempt++ {
		lastErr = putHostKey(relay, token, hostID, body)
		if lastErr == nil {
			return nil
		}
		var statusErr *httpStatusError
		if errors.As(lastErr, &statusErr) &&
			(statusErr.Status == http.StatusUnauthorized || statusErr.Status == http.StatusForbidden) {
			return lastErr
		}
		if attempt < keyReportAttempts {
			time.Sleep(keyReportRetryDelay)
		}
	}
	return lastErr
}

func putHostKey(relay, token, hostID string, body []byte) error {
	req, err := http.NewRequest(http.MethodPut, fmt.Sprintf("%s/api/hosts/%s/key", relay, url.PathEscape(hostID)), bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")

	client := &http.Client{Timeout: 10 * time.Second, Transport: createRelayTransport()}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNoContent || resp.StatusCode == http.StatusOK {
		return nil
	}
	bodyBytes, _ := io.ReadAll(resp.Body)
	debugLog("[key] report failed: status=%d, body=%s", resp.StatusCode, string(bodyBytes))
	return &httpStatusError{Op: "report key", Status: resp.StatusCode}
}

// randRead is a seam so tests can exercise key-generation failures.
var randRead = rand.Read

func generateEncryptionKey() (string, error) {
	key := make([]byte, 32)
	if _, err := randRead(key); err != nil {
		return "", err
	}
	return hex.EncodeToString(key), nil
}

// streamEncryptionKey returns the E2E key used for streaming payloads, or ""
// when the host has no key.
func (c *TunnelClient) streamEncryptionKey() string {
	if c.config == nil {
		return ""
	}
	return c.config.EncryptionKey
}

// streamEncryptionEnabled reports whether streaming payloads must be sealed.
func (c *TunnelClient) streamEncryptionEnabled() bool {
	return c.streamEncryptionKey() != ""
}

// newGCM builds the AES-256-GCM AEAD for a hex key.
func newGCM(keyHex string) (cipher.AEAD, error) {
	key, err := hex.DecodeString(keyHex)
	if err != nil {
		return nil, err
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}

// encryptStreamPayload seals one streaming payload as raw
// nonce(12)||ciphertext||tag(16) bytes. Unlike encrypt, it does not base64 the
// result: streaming payloads travel inside JSON []byte fields, which already
// encode to base64 on the wire (design D8).
func encryptStreamPayload(plaintext []byte, keyHex string) ([]byte, error) {
	gcm, err := newGCM(keyHex)
	if err != nil {
		return nil, err
	}

	nonce := make([]byte, gcm.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return nil, err
	}
	return gcm.Seal(nonce, nonce, plaintext, nil), nil
}

// encryptSSEChunk frames an encrypted SSE chunk as
// len(plaintext, uint32 BE) || nonce(12) || ciphertext || tag(16).
//
// HTTP chunk boundaries are not a reliable framing signal on the client side
// (a single chunk may be split across reads, or coalesced), so the plaintext
// length is carried explicitly. WebSocket frames keep message boundaries and
// use encryptStreamPayload without the prefix.
func encryptSSEChunk(plaintext []byte, keyHex string) ([]byte, error) {
	sealed, err := encryptStreamPayload(plaintext, keyHex)
	if err != nil {
		return nil, err
	}
	out := make([]byte, 4+len(sealed))
	binary.BigEndian.PutUint32(out[:4], uint32(len(plaintext)))
	copy(out[4:], sealed)
	return out, nil
}

// decryptSSEChunk reverses encryptSSEChunk.
func decryptSSEChunk(data []byte, keyHex string) ([]byte, error) {
	if len(data) < 4 {
		return nil, fmt.Errorf("sse chunk too short")
	}
	want := binary.BigEndian.Uint32(data[:4])
	plain, err := decryptStreamPayload(data[4:], keyHex)
	if err != nil {
		return nil, err
	}
	if uint32(len(plain)) != want {
		return nil, fmt.Errorf("sse chunk length mismatch: got %d, want %d", len(plain), want)
	}
	return plain, nil
}

// decryptStreamPayload opens raw nonce(12)||ciphertext||tag(16) bytes produced
// by encryptStreamPayload.
func decryptStreamPayload(ciphertext []byte, keyHex string) ([]byte, error) {
	gcm, err := newGCM(keyHex)
	if err != nil {
		return nil, err
	}

	nonceSize := gcm.NonceSize()
	if len(ciphertext) < nonceSize {
		return nil, fmt.Errorf("ciphertext too short")
	}
	nonce, sealed := ciphertext[:nonceSize], ciphertext[nonceSize:]
	return gcm.Open(nil, nonce, sealed, nil)
}

func encrypt(plaintext []byte, keyHex string) ([]byte, error) {
	key, err := hex.DecodeString(keyHex)
	if err != nil {
		return nil, err
	}

	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}

	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}

	nonce := make([]byte, gcm.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return nil, err
	}

	ciphertext := gcm.Seal(nonce, nonce, plaintext, nil)
	return []byte(base64.StdEncoding.EncodeToString(ciphertext)), nil
}

func decrypt(ciphertext []byte, keyHex string) ([]byte, error) {
	key, err := hex.DecodeString(keyHex)
	if err != nil {
		return nil, err
	}

	data, err := base64.StdEncoding.DecodeString(string(ciphertext))
	if err != nil {
		return nil, err
	}

	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}

	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}

	nonceSize := gcm.NonceSize()
	if len(data) < nonceSize {
		return nil, fmt.Errorf("ciphertext too short")
	}

	nonce, ciphertextBytes := data[:nonceSize], data[nonceSize:]
	return gcm.Open(nil, nonce, ciphertextBytes, nil)
}

// OpenCodeInstance represents a discovered opencode instance
type OpenCodeInstance struct {
	PID     string
	Port    string
	Dir     string
	Healthy bool
}

func cmdDiscover() {
	fmt.Println("\n┌─────────────────────────────────────────────┐")
	fmt.Println("│  OpenCode Discovery                         │")
	fmt.Println("└─────────────────────────────────────────────┘")
	fmt.Println()

	instances := discoverOpenCodeInstances()

	var servers []OpenCodeInstance
	for _, inst := range instances {
		if inst.Port != "" {
			servers = append(servers, inst)
		}
	}

	if len(servers) == 0 {
		fmt.Println("  No opencode servers found.")
		fmt.Println()
		fmt.Println("  Tips:")
		fmt.Println("  - Make sure opencode is running (run 'opencode' in a project directory)")
		fmt.Println("  - Check if the server has started (wait a few seconds)")
		return
	}

	fmt.Printf("  Found %d opencode server(s):\n\n", len(servers))

	for i, inst := range servers {
		status := "✓ healthy"
		if !inst.Healthy {
			status = "✗ unreachable"
		}
		fmt.Printf("  %d. %s (port %s)\n", i+1, status, inst.Port)

		if inst.Healthy {
			projects := fetchProjects(inst.Port)
			if len(projects) > 0 {
				fmt.Printf("     Projects (%d):\n", len(projects))
				for _, p := range projects {
					if p.Worktree == "/" {
						fmt.Printf("       - global\n")
					} else {
						fmt.Printf("       - %s\n", p.Worktree)
					}
				}
			}
		}
		fmt.Println()
	}
}

type ProjectInfo struct {
	ID       string `json:"id"`
	Worktree string `json:"worktree"`
}

func fetchProjects(port string) []ProjectInfo {
	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Get(fmt.Sprintf("http://127.0.0.1:%s/project", port))
	if err != nil {
		return nil
	}
	defer resp.Body.Close()

	var projects []ProjectInfo
	if err := json.NewDecoder(resp.Body).Decode(&projects); err != nil {
		return nil
	}
	return projects
}

// discoverOpenCodeInstances finds all running opencode instances with their ports and directories
func discoverOpenCodeInstances() []OpenCodeInstance {
	pids := getOpenCodePIDs()
	if len(pids) == 0 {
		return nil
	}

	debugLog("[discover] Found %d opencode PIDs: %v", len(pids), pids)

	pidSet := make(map[string]bool)
	for _, pid := range pids {
		pidSet[pid] = true
	}

	portMap := getOpenCodeListeningPorts(pidSet)
	cwdMap := getOpenCodeCwds(pids)

	var instances []OpenCodeInstance
	for _, pid := range pids {
		inst := OpenCodeInstance{
			PID:  pid,
			Port: portMap[pid],
			Dir:  cwdMap[pid],
		}

		if inst.Port != "" {
			inst.Healthy = checkOpenCodeHealth(inst.Port)
		}

		instances = append(instances, inst)
	}

	return instances
}

func getOpenCodeListeningPorts(pidSet map[string]bool) map[string]string {
	result := make(map[string]string)

	if runtime.GOOS == "windows" {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()

		cmd := exec.CommandContext(ctx, "cmd", "/c", "netstat -ano | findstr LISTENING")
		output, err := cmd.Output()
		if err != nil {
			debugLog("[discover] netstat failed: %v", err)
			return result
		}

		lines := strings.Split(string(output), "\n")
		for _, line := range lines {
			fields := strings.Fields(line)
			if len(fields) < 5 {
				continue
			}
			pid := fields[len(fields)-1]
			if !pidSet[pid] {
				continue
			}
			addr := fields[1]
			parts := strings.Split(addr, ":")
			if len(parts) >= 2 {
				result[pid] = parts[len(parts)-1]
			}
		}
		return result
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	cmd := exec.CommandContext(ctx, "lsof", "-i", "-P", "-n")
	output, err := cmd.Output()
	if err != nil {
		debugLog("[discover] lsof -i failed: %v", err)
		return result
	}

	lines := strings.Split(string(output), "\n")
	for _, line := range lines {
		if !strings.Contains(line, "LISTEN") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 9 || fields[0] != "opencode" {
			continue
		}
		pid := fields[1]
		if !pidSet[pid] {
			continue
		}
		addr := fields[8]
		parts := strings.Split(addr, ":")
		if len(parts) >= 2 {
			result[pid] = parts[len(parts)-1]
		}
	}

	return result
}

func getOpenCodeCwds(pids []string) map[string]string {
	result := make(map[string]string)

	if runtime.GOOS == "windows" {
		for _, pid := range pids {
			if cwd := getProcessCwdWindows(pid); cwd != "" {
				result[pid] = cwd
			}
		}
		return result
	}

	if runtime.GOOS == "linux" {
		for _, pid := range pids {
			cwdLink := fmt.Sprintf("/proc/%s/cwd", pid)
			if cwd, err := os.Readlink(cwdLink); err == nil {
				result[pid] = cwd
			}
		}
		return result
	}

	for _, pid := range pids {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		cmd := exec.CommandContext(ctx, "lsof", "-a", "-d", "cwd", "-p", pid, "-Fn")
		output, err := cmd.Output()
		cancel()
		if err != nil {
			continue
		}

		lines := strings.Split(string(output), "\n")
		for _, line := range lines {
			if strings.HasPrefix(line, "n") {
				result[pid] = line[1:]
				break
			}
		}
	}

	return result
}

func getProcessCwdWindows(pid string) string {
	return ""
}

// checkOpenCodeHealth verifies if an opencode instance is responding
func checkOpenCodeHealth(port string) bool {
	client := &http.Client{
		Timeout: 5 * time.Second,
		Transport: &http.Transport{
			DisableKeepAlives: true,
		},
	}
	url := fmt.Sprintf("http://127.0.0.1:%s", port)

	resp, err := client.Get(url)
	if err != nil {
		debugLog("[discover] health check failed for port %s: %v", port, err)
		return false
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, resp.Body)
	return resp.StatusCode >= 200 && resp.StatusCode < 500
}
