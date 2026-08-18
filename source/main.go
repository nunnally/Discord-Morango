package main

import (
	"bufio"
	"crypto/tls"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"math/rand"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"time"
)

const (
	freeProxyAPI       = "https://api.proxyscrape.com/v4/free-proxy-list/get"
	targetHost         = "gateway.discord.gg"
	targetPort         = 443
	maxProxyCandidates = 8
	proxyTestTimeout   = 5 * time.Second
	maxListBytes       = 1 << 20
)

type ProxySpec struct {
	Scheme string
	Host   string
	Port   int
	Raw    string
}

type ProxyEntry struct {
	Proxy       string
	CountryCode string
}

type configFile struct {
	LastKnownProxy string `json:"lastKnownProxy,omitempty"`
}

type freeProxyResponse struct {
	Proxies []struct {
		Proxy  string `json:"proxy"`
		IPData *struct {
			CountryCode string `json:"countryCode"`
		} `json:"ip_data"`
	} `json:"proxies"`
}

type options struct {
	Protocol    string
	Excluded    string
	ManualProxy string
	DiscordBin  string
	StateDir    string
	ReuseLast   bool
	Direct      bool
	Relay       bool
	ListenPort  int
	Help        bool
}

func parseProxy(raw string) (ProxySpec, error) {
	raw = strings.TrimSpace(raw)
	u, err := url.Parse(raw)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return ProxySpec{}, errors.New("invalid proxy format; use scheme://host:port")
	}
	scheme := strings.ToLower(u.Scheme)
	if scheme != "http" && scheme != "socks4" && scheme != "socks5" {
		return ProxySpec{}, errors.New("unsupported proxy protocol")
	}
	if u.User != nil || u.Path != "" || u.RawQuery != "" || u.Fragment != "" {
		return ProxySpec{}, errors.New("proxy must be scheme://host:port without credentials/path")
	}
	host := u.Hostname()
	p, err := strconv.Atoi(u.Port())
	if err != nil || p < 1 || p > 65535 || host == "" {
		return ProxySpec{}, errors.New("invalid proxy port")
	}
	normalized := fmt.Sprintf("%s://%s:%d", scheme, hostForURL(host), p)
	return ProxySpec{Scheme: scheme, Host: host, Port: p, Raw: normalized}, nil
}

func hostForURL(host string) string {
	if strings.Contains(host, ":") && !strings.HasPrefix(host, "[") {
		return "[" + host + "]"
	}
	return host
}

func makePAC(host string, port int) string {
	return fmt.Sprintf(`function FindProxyForURL(url, host) {
  if (host === %q) {
    return "PROXY %s:%d";
  }
  return "DIRECT";
}
`, targetHost, host, port)
}

func filterEntries(entries []ProxyEntry, excluded map[string]bool) []ProxyEntry {
	out := make([]ProxyEntry, 0, len(entries))
	for _, e := range entries {
		cc := strings.ToUpper(strings.TrimSpace(e.CountryCode))
		if cc != "" && excluded[cc] {
			continue
		}
		out = append(out, e)
	}
	return out
}

func stateDirDefault() string {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return ".discord-golive"
	}
	return filepath.Join(home, ".discord-golive")
}

func parseOptions(args []string) (options, error) {
	var o options
	fs := flag.NewFlagSet("discord-golive", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	fs.StringVar(&o.Protocol, "protocol", envOr("GOLIVE_PROTOCOL", "socks5"), "proxy protocol")
	fs.StringVar(&o.Excluded, "exclude", envOr("GOLIVE_EXCLUDE", "BR"), "excluded country codes")
	fs.StringVar(&o.ManualProxy, "proxy", os.Getenv("GOLIVE_PROXY"), "manual proxy")
	fs.StringVar(&o.DiscordBin, "discord-bin", os.Getenv("DISCORD_BIN"), "Discord executable")
	fs.StringVar(&o.StateDir, "state-dir", envOr("GOLIVE_STATE_DIR", stateDirDefault()), "state directory")
	fs.BoolVar(&o.Direct, "direct", false, "switch existing relay to DIRECT for new connections")
	fs.BoolVar(&o.Relay, "relay", false, "internal relay mode")
	fs.IntVar(&o.ListenPort, "listen-port", 0, "internal relay listen port")
	fs.BoolVar(&o.Help, "help", false, "help")
	fs.BoolVar(&o.Help, "h", false, "help")
	noReuse := fs.Bool("no-reuse", false, "do not reuse last proxy")
	if err := fs.Parse(args); err != nil {
		return o, err
	}
	o.Protocol = strings.ToLower(strings.TrimSpace(o.Protocol))
	if o.Protocol != "http" && o.Protocol != "socks4" && o.Protocol != "socks5" {
		return o, fmt.Errorf("unsupported protocol %q", o.Protocol)
	}
	o.ReuseLast = !*noReuse
	return o, nil
}

func envOr(key, fallback string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return fallback
}

func excludedSet(raw string) map[string]bool {
	out := map[string]bool{}
	for _, part := range strings.Split(raw, ",") {
		p := strings.ToUpper(strings.TrimSpace(part))
		if len(p) == 2 {
			out[p] = true
		}
	}
	return out
}

func loadConfig(path string) configFile {
	data, err := os.ReadFile(path)
	if err != nil {
		return configFile{}
	}
	var cfg configFile
	if json.Unmarshal(data, &cfg) != nil {
		return configFile{}
	}
	return cfg
}

func saveConfig(path string, cfg configFile) error {
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	return os.WriteFile(path, data, 0600)
}

func fetchProxyEntries(protocol string) ([]ProxyEntry, error) {
	q := url.Values{}
	q.Set("request", "display_proxies")
	q.Set("protocol", protocol)
	q.Set("proxy_format", "protocolipport")
	q.Set("format", "json")
	q.Set("timeout", "5000")

	client := &http.Client{Timeout: 15 * time.Second}
	req, err := http.NewRequest(http.MethodGet, freeProxyAPI+"?"+q.Encode(), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "discord-golive-go/1.0")
	res, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("proxy list returned HTTP %d", res.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(res.Body, maxListBytes+1))
	if err != nil {
		return nil, err
	}
	if len(body) > maxListBytes {
		return nil, errors.New("proxy list response too large")
	}
	var payload freeProxyResponse
	if err := json.Unmarshal(body, &payload); err != nil {
		return nil, err
	}
	entries := make([]ProxyEntry, 0, len(payload.Proxies))
	for _, p := range payload.Proxies {
		spec, err := parseProxy(p.Proxy)
		if err != nil || spec.Scheme != protocol {
			continue
		}
		cc := ""
		if p.IPData != nil {
			cc = strings.ToUpper(p.IPData.CountryCode)
		}
		entries = append(entries, ProxyEntry{Proxy: spec.Raw, CountryCode: cc})
	}
	return entries, nil
}

func dialTCP(host string, port int, timeout time.Duration) (net.Conn, error) {
	return net.DialTimeout("tcp", net.JoinHostPort(host, strconv.Itoa(port)), timeout)
}

func openProxyTunnel(spec ProxySpec, host string, port int) (net.Conn, error) {
	conn, err := dialTCP(spec.Host, spec.Port, proxyTestTimeout)
	if err != nil {
		return nil, err
	}
	_ = conn.SetDeadline(time.Now().Add(proxyTestTimeout))
	fail := func(err error) (net.Conn, error) {
		_ = conn.Close()
		return nil, err
	}

	switch spec.Scheme {
	case "http":
		if _, err := fmt.Fprintf(conn, "CONNECT %s HTTP/1.1\r\nHost: %s\r\nProxy-Connection: keep-alive\r\n\r\n", net.JoinHostPort(host, strconv.Itoa(port)), net.JoinHostPort(host, strconv.Itoa(port))); err != nil {
			return fail(err)
		}
		br := bufio.NewReader(conn)
		status, err := br.ReadString('\n')
		if err != nil {
			return fail(err)
		}
		if !strings.Contains(status, " 200 ") {
			return fail(fmt.Errorf("HTTP proxy rejected CONNECT: %s", strings.TrimSpace(status)))
		}
		for {
			line, err := br.ReadString('\n')
			if err != nil {
				return fail(err)
			}
			if line == "\r\n" {
				break
			}
			if len(line) > 16384 {
				return fail(errors.New("proxy response headers too large"))
			}
		}
	case "socks5":
		if _, err := conn.Write([]byte{0x05, 0x01, 0x00}); err != nil {
			return fail(err)
		}
		resp := make([]byte, 2)
		if _, err := io.ReadFull(conn, resp); err != nil {
			return fail(err)
		}
		if resp[0] != 0x05 || resp[1] != 0x00 {
			return fail(errors.New("SOCKS5 proxy requires unsupported authentication"))
		}
		hb := []byte(host)
		if len(hb) > 255 {
			return fail(errors.New("target host too long"))
		}
		req := []byte{0x05, 0x01, 0x00, 0x03, byte(len(hb))}
		req = append(req, hb...)
		req = append(req, byte(port>>8), byte(port))
		if _, err := conn.Write(req); err != nil {
			return fail(err)
		}
		head := make([]byte, 4)
		if _, err := io.ReadFull(conn, head); err != nil {
			return fail(err)
		}
		if head[0] != 0x05 || head[1] != 0x00 {
			return fail(fmt.Errorf("SOCKS5 CONNECT failed with code %d", head[1]))
		}
		switch head[3] {
		case 0x01:
			_, err = io.CopyN(io.Discard, conn, 6)
		case 0x03:
			one := make([]byte, 1)
			if _, err = io.ReadFull(conn, one); err == nil {
				_, err = io.CopyN(io.Discard, conn, int64(one[0])+2)
			}
		case 0x04:
			_, err = io.CopyN(io.Discard, conn, 18)
		default:
			err = errors.New("SOCKS5 proxy returned invalid address type")
		}
		if err != nil {
			return fail(err)
		}
	case "socks4":
		hb := []byte(host)
		req := []byte{0x04, 0x01, byte(port >> 8), byte(port), 0, 0, 0, 1, 0}
		req = append(req, hb...)
		req = append(req, 0)
		if _, err := conn.Write(req); err != nil {
			return fail(err)
		}
		resp := make([]byte, 8)
		if _, err := io.ReadFull(conn, resp); err != nil {
			return fail(err)
		}
		if resp[1] != 0x5a {
			return fail(fmt.Errorf("SOCKS4a CONNECT failed with code %d", resp[1]))
		}
	default:
		return fail(errors.New("unsupported proxy scheme"))
	}
	_ = conn.SetDeadline(time.Time{})
	return conn, nil
}

func testProxy(spec ProxySpec) bool {
	conn, err := openProxyTunnel(spec, targetHost, targetPort)
	if err != nil {
		return false
	}
	tlsConn := tls.Client(conn, &tls.Config{ServerName: targetHost, MinVersion: tls.VersionTLS12})
	_ = tlsConn.SetDeadline(time.Now().Add(proxyTestTimeout))
	err = tlsConn.Handshake()
	_ = tlsConn.Close()
	return err == nil
}

func chooseProxy(o options, cfgPath string) (ProxySpec, error) {
	if o.ManualProxy != "" {
		spec, err := parseProxy(o.ManualProxy)
		if err != nil {
			return ProxySpec{}, err
		}
		fmt.Printf("[proxy] Testando proxy manual %s ... ", spec.Raw)
		if !testProxy(spec) {
			fmt.Println("falhou")
			return ProxySpec{}, errors.New("proxy manual não conseguiu abrir TLS com gateway.discord.gg:443")
		}
		fmt.Println("OK")
		_ = saveConfig(cfgPath, configFile{LastKnownProxy: spec.Raw})
		return spec, nil
	}

	cfg := loadConfig(cfgPath)
	if o.ReuseLast && cfg.LastKnownProxy != "" {
		if spec, err := parseProxy(cfg.LastKnownProxy); err == nil && spec.Scheme == o.Protocol {
			fmt.Printf("[proxy] Testando última proxy funcional %s ... ", spec.Raw)
			if testProxy(spec) {
				fmt.Println("OK")
				return spec, nil
			}
			fmt.Println("falhou")
		}
	}

	fmt.Printf("[proxy] Buscando proxies %s; excluindo %s...\n", o.Protocol, o.Excluded)
	entries, err := fetchProxyEntries(o.Protocol)
	if err != nil {
		return ProxySpec{}, fmt.Errorf("falha ao buscar lista de proxies: %w", err)
	}
	entries = filterEntries(entries, excludedSet(o.Excluded))
	if len(entries) == 0 {
		return ProxySpec{}, errors.New("lista gratuita não retornou proxies compatíveis fora dos países excluídos")
	}

	r := rand.New(rand.NewSource(time.Now().UnixNano()))
	r.Shuffle(len(entries), func(i, j int) { entries[i], entries[j] = entries[j], entries[i] })
	if len(entries) > maxProxyCandidates {
		entries = entries[:maxProxyCandidates]
	}
	for i, entry := range entries {
		spec, err := parseProxy(entry.Proxy)
		if err != nil {
			continue
		}
		suffix := ""
		if entry.CountryCode != "" {
			suffix = " [" + entry.CountryCode + "]"
		}
		fmt.Printf("[proxy] Testando %d/%d: %s%s ... ", i+1, len(entries), spec.Raw, suffix)
		if testProxy(spec) {
			fmt.Println("OK")
			_ = saveConfig(cfgPath, configFile{LastKnownProxy: spec.Raw})
			return spec, nil
		}
		fmt.Println("falhou")
	}
	return ProxySpec{}, errors.New("nenhuma proxy funcional foi encontrada entre as candidatas testadas")
}

func modePath(stateDir string) string { return filepath.Join(stateDir, "mode") }

func readMode(stateDir string) string {
	data, err := os.ReadFile(modePath(stateDir))
	if err == nil && strings.TrimSpace(string(data)) == "direct" {
		return "direct"
	}
	return "proxy"
}

func writeMode(stateDir, mode string) error {
	if mode != "proxy" && mode != "direct" {
		return errors.New("invalid relay mode")
	}
	if err := os.MkdirAll(stateDir, 0700); err != nil {
		return err
	}
	return os.WriteFile(modePath(stateDir), []byte(mode+"\n"), 0600)
}

func freePort() (int, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	defer ln.Close()
	return ln.Addr().(*net.TCPAddr).Port, nil
}

func waitPort(port int, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		c, err := net.DialTimeout("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)), 200*time.Millisecond)
		if err == nil {
			_ = c.Close()
			return nil
		}
		time.Sleep(100 * time.Millisecond)
	}
	return errors.New("relay local não iniciou a tempo")
}

func parseConnectTarget(raw string) (string, int, error) {
	host, portRaw, err := net.SplitHostPort(raw)
	if err != nil {
		return "", 0, err
	}
	port, err := strconv.Atoi(portRaw)
	if err != nil || port < 1 || port > 65535 {
		return "", 0, errors.New("invalid target port")
	}
	return host, port, nil
}

func relayMain(o options) error {
	if o.ListenPort < 1 || o.ListenPort > 65535 {
		return errors.New("invalid relay listen port")
	}
	spec, err := parseProxy(o.ManualProxy)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(o.StateDir, 0700); err != nil {
		return err
	}
	_ = writeMode(o.StateDir, "proxy")
	_ = os.WriteFile(filepath.Join(o.StateDir, "relay.pid"), []byte(strconv.Itoa(os.Getpid())+"\n"), 0600)
	_ = os.WriteFile(filepath.Join(o.StateDir, "relay.port"), []byte(strconv.Itoa(o.ListenPort)+"\n"), 0600)

	pac := makePAC("127.0.0.1", o.ListenPort)
	mux := http.NewServeMux()
	mux.HandleFunc("/proxy.pac", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/x-ns-proxy-autoconfig")
		w.Header().Set("Cache-Control", "no-store, no-cache, must-revalidate")
		_, _ = io.WriteString(w, pac)
	})
	mux.HandleFunc("/status", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{"mode": readMode(o.StateDir), "target": targetHost})
	})

	server := &http.Server{Addr: net.JoinHostPort("127.0.0.1", strconv.Itoa(o.ListenPort)), Handler: mux}
	server.Handler = connectHandler{next: mux, stateDir: o.StateDir, proxy: spec}

	go monitorDiscord(o.StateDir, server)
	fmt.Printf("[relay] ouvindo em 127.0.0.1:%d; modo inicial PROXY\n", o.ListenPort)
	err = server.ListenAndServe()
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}

type connectHandler struct {
	next     http.Handler
	stateDir string
	proxy    ProxySpec
}

func (h connectHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodConnect {
		h.next.ServeHTTP(w, r)
		return
	}
	host, port, err := parseConnectTarget(r.Host)
	if err != nil || !strings.EqualFold(host, targetHost) || port != targetPort {
		http.Error(w, "Forbidden", http.StatusForbidden)
		return
	}

	hj, ok := w.(http.Hijacker)
	if !ok {
		http.Error(w, "Hijacking unsupported", http.StatusInternalServerError)
		return
	}
	client, _, err := hj.Hijack()
	if err != nil {
		return
	}

	mode := readMode(h.stateDir)
	var upstream net.Conn
	if mode == "proxy" {
		upstream, err = openProxyTunnel(h.proxy, host, port)
	} else {
		upstream, err = dialTCP(host, port, 8*time.Second)
	}
	if err != nil {
		_, _ = io.WriteString(client, "HTTP/1.1 502 Bad Gateway\r\n\r\n")
		_ = client.Close()
		return
	}
	_, _ = io.WriteString(client, "HTTP/1.1 200 Connection Established\r\nProxy-Agent: discord-golive-go\r\n\r\n")
	fmt.Printf("[relay] nova conexão do Gateway: %s\n", strings.ToUpper(mode))

	go proxyCopy(client, upstream)
	go proxyCopy(upstream, client)
}

func proxyCopy(dst, src net.Conn) {
	_, _ = io.Copy(dst, src)
	_ = dst.Close()
	_ = src.Close()
}

func monitorDiscord(stateDir string, server *http.Server) {
	seen := false
	missing := 0
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()
	for range ticker.C {
		data, err := os.ReadFile(filepath.Join(stateDir, "discord.pid"))
		if err != nil {
			continue
		}
		pid, _ := strconv.Atoi(strings.TrimSpace(string(data)))
		if pid <= 0 {
			continue
		}
		if processAlive(pid) {
			seen = true
			missing = 0
			continue
		}
		if seen {
			missing++
			if missing >= 3 {
				_ = server.Close()
				cleanupStateFiles(stateDir)
				return
			}
		}
	}
}

func cleanupStateFiles(stateDir string) {
	for _, name := range []string{"relay.pid", "relay.port", "discord.pid", "debug.port", "mode"} {
		_ = os.Remove(filepath.Join(stateDir, name))
	}
}

func stopExistingRelay(stateDir string) {
	data, _ := os.ReadFile(filepath.Join(stateDir, "relay.pid"))
	pid, _ := strconv.Atoi(strings.TrimSpace(string(data)))
	if pid > 0 && processAlive(pid) {
		terminateProcess(pid)
		time.Sleep(300 * time.Millisecond)
	}
	cleanupStateFiles(stateDir)
}

func startRelay(exe string, port int, spec ProxySpec, stateDir string) (int, error) {
	if err := os.MkdirAll(stateDir, 0700); err != nil {
		return 0, err
	}
	logPath := filepath.Join(stateDir, "relay.log")
	logFile, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0600)
	if err != nil {
		return 0, err
	}
	defer logFile.Close()
	cmd := exec.Command(exe, "--relay", "--listen-port", strconv.Itoa(port), "--proxy", spec.Raw, "--state-dir", stateDir)
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	configureDetached(cmd)
	if err := cmd.Start(); err != nil {
		return 0, err
	}
	return cmd.Process.Pid, nil
}

func findDiscordExecutable(override string) (string, error) {
	if strings.TrimSpace(override) != "" {
		if _, err := os.Stat(override); err != nil {
			return "", err
		}
		return override, nil
	}
	if runtime.GOOS == "darwin" {
		path := "/Applications/Discord.app/Contents/MacOS/Discord"
		if _, err := os.Stat(path); err != nil {
			return "", fmt.Errorf("Discord não encontrado em %s; use --discord-bin", path)
		}
		return path, nil
	}
	if runtime.GOOS == "windows" {
		local := os.Getenv("LOCALAPPDATA")
		if local == "" {
			return "", errors.New("LOCALAPPDATA não encontrado")
		}
		root := filepath.Join(local, "Discord")
		matches, _ := filepath.Glob(filepath.Join(root, "app-*", "Discord.exe"))
		if len(matches) == 0 {
			return "", fmt.Errorf("Discord.exe não encontrado em %s; use --discord-bin", root)
		}
		sort.Strings(matches)
		return matches[len(matches)-1], nil
	}
	return "", fmt.Errorf("sistema operacional não suportado: %s", runtime.GOOS)
}

func closeDiscord() {
	if runtime.GOOS == "darwin" {
		_ = exec.Command("osascript", "-e", `quit app "Discord"`).Run()
		_ = exec.Command("pkill", "-x", "Discord").Run()
		_ = exec.Command("pkill", "-f", "Discord Helper").Run()
	} else if runtime.GOOS == "windows" {
		_ = runHiddenCommand("taskkill", "/IM", "Discord.exe", "/F", "/T")
	}
	time.Sleep(1200 * time.Millisecond)
}

func buildDiscordArgs(pacURL string, debugPort int) []string {
	return []string{
		"--proxy-pac-url=" + pacURL,
		"--remote-debugging-port=" + strconv.Itoa(debugPort),
	}
}

func startDiscord(bin, pacURL, stateDir string, debugPort int) (int, error) {
	logPath := filepath.Join(stateDir, "discord.log")
	logFile, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0600)
	if err != nil {
		return 0, err
	}
	defer logFile.Close()
	cmd := exec.Command(bin, buildDiscordArgs(pacURL, debugPort)...)
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	configureDetached(cmd)
	if err := cmd.Start(); err != nil {
		return 0, err
	}
	pid := cmd.Process.Pid
	_ = os.WriteFile(filepath.Join(stateDir, "discord.pid"), []byte(strconv.Itoa(pid)+"\n"), 0600)
	_ = os.WriteFile(filepath.Join(stateDir, "debug.port"), []byte(strconv.Itoa(debugPort)+"\n"), 0600)
	return pid, nil
}

func switchExistingToDirect(stateDir string) error {
	data, err := os.ReadFile(filepath.Join(stateDir, "relay.pid"))
	if err != nil {
		return errors.New("nenhum relay ativo encontrado")
	}
	pid, _ := strconv.Atoi(strings.TrimSpace(string(data)))
	if pid <= 0 || !processAlive(pid) {
		return errors.New("relay salvo não está mais rodando")
	}
	return writeMode(stateDir, "direct")
}

func launcherMain(o options) error {
	fmt.Println("==========================================")
	fmt.Println(" Discord Go Live Launcher — Go v3")
	fmt.Println("==========================================")
	fmt.Printf("[config] Sistema: %s/%s\n", runtime.GOOS, runtime.GOARCH)
	fmt.Printf("[config] Protocolo: %s\n", o.Protocol)
	fmt.Printf("[config] Países excluídos: %s\n", o.Excluded)
	fmt.Println("[config] Proxy global do sistema: NÃO será alterado")
	fmt.Println("[aviso] Proxies gratuitos são instáveis; o operador vê metadados da conexão, embora o Gateway use TLS.")

	if o.Direct {
		if err := switchExistingToDirect(o.StateDir); err != nil {
			return err
		}
		fmt.Println("[OK] NOVAS conexões do Gateway agora são DIRECT.")
		fmt.Println("[OK] A conexão já aberta não é interrompida.")
		return nil
	}

	if err := os.MkdirAll(o.StateDir, 0700); err != nil {
		return err
	}
	stopExistingRelay(o.StateDir)
	cfgPath := filepath.Join(o.StateDir, "config.json")
	spec, err := chooseProxy(o, cfgPath)
	if err != nil {
		return err
	}
	fmt.Printf("[proxy] Selecionada: %s\n", spec.Raw)

	discordBin, err := findDiscordExecutable(o.DiscordBin)
	if err != nil {
		return err
	}
	fmt.Printf("[discord] Executável: %s\n", discordBin)

	port, err := freePort()
	if err != nil {
		return err
	}
	if err := writeMode(o.StateDir, "proxy"); err != nil {
		return err
	}
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	relayPID, err := startRelay(exe, port, spec, o.StateDir)
	if err != nil {
		return fmt.Errorf("falha ao iniciar relay: %w", err)
	}
	if err := waitPort(port, 8*time.Second); err != nil {
		terminateProcess(relayPID)
		return err
	}
	pacURL := fmt.Sprintf("http://127.0.0.1:%d/proxy.pac", port)
	fmt.Printf("[relay] PID %d; PAC %s\n", relayPID, pacURL)

	debugPort, err := freePort()
	if err != nil {
		terminateProcess(relayPID)
		return err
	}
	closeDiscord()
	discordPID, err := startDiscord(discordBin, pacURL, o.StateDir, debugPort)
	if err != nil {
		terminateProcess(relayPID)
		return fmt.Errorf("falha ao abrir Discord: %w", err)
	}
	fmt.Printf("[discord] PID %d\n", discordPID)
	fmt.Println("[discord] gateway.discord.gg nasce pela proxy; demais hosts usam DIRECT.")
	fmt.Printf("[hook] CDP local em 127.0.0.1:%d; procurando renderer do Discord...\n", debugPort)

	target, hookErr := waitForDiscordTarget(debugPort, 30*time.Second)
	if hookErr == nil {
		fmt.Printf("[hook] Renderer encontrado: %s\n", target.URL)
		fmt.Println("[hook] Automático: entre em um canal de voz; vou aguardar VIDEO + DESKTOP_CAPTURE ficarem liberados.")
		fmt.Println("[hook] Fallback opcional: pressione ENTER a qualquer momento para forçar DIRECT manualmente.")
		watchCh := make(chan error, 1)
		manualCh := make(chan struct{}, 1)
		go func() {
			watchCh <- watchGoLiveGate(target, 30*time.Minute, func(format string, args ...any) { fmt.Printf(format+"\n", args...) })
		}()
		go func() {
			_, _ = bufio.NewReader(os.Stdin).ReadString('\n')
			manualCh <- struct{}{}
		}()
		select {
		case hookErr = <-watchCh:
			if hookErr != nil {
				fmt.Printf("[hook] Automação não concluiu: %v\n", hookErr)
				fmt.Println("[fallback] Pressione ENTER para mudar NOVAS conexões para DIRECT.")
				<-manualCh
			}
		case <-manualCh:
			fmt.Println("[fallback] DIRECT solicitado manualmente.")
			hookErr = nil
		}
	} else {
		fmt.Printf("[hook] Não consegui conectar ao renderer: %v\n", hookErr)
		fmt.Println("[fallback] Entre no canal de voz e pressione ENTER para mudar NOVAS conexões para DIRECT.")
		fmt.Print("> ")
		_, _ = bufio.NewReader(os.Stdin).ReadString('\n')
	}

	if err := writeMode(o.StateDir, "direct"); err != nil {
		return err
	}
	fmt.Println()
	fmt.Println("[OK] Gate observado; NOVAS conexões do Gateway agora são DIRECT.")
	fmt.Println("[OK] O WebSocket que já nasceu pela proxy continua nessa rota até reconectar.")
	fmt.Println("[OK] O relay permanece em background somente para sustentar conexões existentes.")
	fmt.Println("[OK] O Discord continua aberto. Pode fechar este Terminal.")
	return nil
}

func printHelp() {
	fmt.Println(`Discord Go Live Launcher — Go v3

Uso:
  discord-golive [opções]

Opções:
  --protocol socks5|socks4|http   Protocolo de proxy (padrão: socks5)
  --exclude BR,XX                 Países excluídos da lista gratuita (padrão: BR)
  --proxy scheme://host:port      Usa proxy manual
  --no-reuse                      Não reutiliza a última proxy funcional
  --discord-bin CAMINHO           Caminho manual do Discord
  --direct                        Faz um relay existente usar DIRECT em novas conexões
  --help                          Mostra esta ajuda

O utilitário NÃO altera o proxy global do macOS/Windows.`)
}

func main() {
	o, err := parseOptions(os.Args[1:])
	if err != nil {
		fmt.Fprintln(os.Stderr, "ERRO:", err)
		os.Exit(2)
	}
	if o.Help {
		printHelp()
		return
	}
	if o.Relay {
		err = relayMain(o)
	} else {
		err = launcherMain(o)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "ERRO:", err)
		os.Exit(1)
	}
}
