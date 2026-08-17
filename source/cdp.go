package main

import (
	"bufio"
	"crypto/rand"
	"crypto/sha1"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

type cdpTarget struct {
	ID                   string `json:"id"`
	Type                 string `json:"type"`
	Title                string `json:"title"`
	URL                  string `json:"url"`
	WebSocketDebuggerURL string `json:"webSocketDebuggerUrl"`
}

func chooseDiscordTarget(targets []cdpTarget) int {
	best, score := -1, -1
	for i, t := range targets {
		if t.WebSocketDebuggerURL == "" || t.Type != "page" {
			continue
		}
		s := 0
		lu := strings.ToLower(t.URL)
		lt := strings.ToLower(t.Title)
		if strings.Contains(lu, "discord.com/channels") {
			s += 100
		}
		if strings.Contains(lu, "discord.com") {
			s += 30
		}
		if strings.Contains(lt, "discord") {
			s += 20
		}
		if s > score {
			best, score = i, s
		}
	}
	return best
}

func fetchCDPTargets(port int) ([]cdpTarget, error) {
	client := &http.Client{Timeout: 2 * time.Second}
	res, err := client.Get(fmt.Sprintf("http://127.0.0.1:%d/json/list", port))
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("CDP returned HTTP %d", res.StatusCode)
	}
	var out []cdpTarget
	if err := json.NewDecoder(io.LimitReader(res.Body, 1<<20)).Decode(&out); err != nil {
		return nil, err
	}
	return out, nil
}

func waitForDiscordTarget(port int, timeout time.Duration) (cdpTarget, error) {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		targets, err := fetchCDPTargets(port)
		if err == nil {
			// Prefer targets that really expose the Discord webpack global.
			order := make([]int, 0, len(targets))
			if idx := chooseDiscordTarget(targets); idx >= 0 {
				order = append(order, idx)
			}
			for i := range targets {
				if targets[i].Type == "page" && targets[i].WebSocketDebuggerURL != "" && (len(order) == 0 || i != order[0]) {
					order = append(order, i)
				}
			}
			for _, idx := range order {
				ok, _ := targetHasDiscordWebpack(targets[idx])
				if ok {
					return targets[idx], nil
				}
			}
		}
		time.Sleep(250 * time.Millisecond)
	}
	return cdpTarget{}, errors.New("renderer do Discord não apareceu no CDP a tempo")
}

type wsClient struct {
	conn net.Conn
	br   *bufio.Reader
	mu   sync.Mutex
}

func dialWS(rawURL string, timeout time.Duration) (*wsClient, error) {
	u, err := url.Parse(rawURL)
	if err != nil || u.Scheme != "ws" {
		return nil, errors.New("CDP websocket URL inválida")
	}
	host := u.Host
	if !strings.Contains(host, ":") {
		host += ":80"
	}
	conn, err := net.DialTimeout("tcp", host, timeout)
	if err != nil {
		return nil, err
	}
	br := bufio.NewReader(conn)

	keyBytes := make([]byte, 16)
	if _, err := rand.Read(keyBytes); err != nil {
		conn.Close()
		return nil, err
	}
	key := base64.StdEncoding.EncodeToString(keyBytes)
	path := u.RequestURI()
	if path == "" {
		path = "/"
	}
	req := fmt.Sprintf("GET %s HTTP/1.1\r\nHost: %s\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Key: %s\r\nSec-WebSocket-Version: 13\r\n\r\n", path, u.Host, key)
	if _, err := io.WriteString(conn, req); err != nil {
		conn.Close()
		return nil, err
	}
	_ = conn.SetDeadline(time.Now().Add(timeout))
	status, err := br.ReadString('\n')
	if err != nil {
		conn.Close()
		return nil, err
	}
	if !strings.Contains(status, " 101 ") {
		conn.Close()
		return nil, fmt.Errorf("websocket upgrade recusado: %s", strings.TrimSpace(status))
	}
	headers := map[string]string{}
	for {
		line, err := br.ReadString('\n')
		if err != nil {
			conn.Close()
			return nil, err
		}
		if line == "\r\n" {
			break
		}
		parts := strings.SplitN(strings.TrimSpace(line), ":", 2)
		if len(parts) == 2 {
			headers[strings.ToLower(strings.TrimSpace(parts[0]))] = strings.TrimSpace(parts[1])
		}
	}
	acceptRaw := sha1.Sum([]byte(key + "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"))
	expected := base64.StdEncoding.EncodeToString(acceptRaw[:])
	if got := headers["sec-websocket-accept"]; got != "" && got != expected {
		conn.Close()
		return nil, errors.New("websocket accept inválido")
	}
	_ = conn.SetDeadline(time.Time{})
	return &wsClient{conn: conn, br: br}, nil
}

func (w *wsClient) Close() error { return w.conn.Close() }

func (w *wsClient) writeFrame(opcode byte, payload []byte) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	mask := make([]byte, 4)
	if _, err := rand.Read(mask); err != nil {
		return err
	}
	header := []byte{0x80 | opcode}
	n := len(payload)
	switch {
	case n < 126:
		header = append(header, 0x80|byte(n))
	case n <= 65535:
		header = append(header, 0x80|126, byte(n>>8), byte(n))
	default:
		header = append(header, 0x80|127)
		var b [8]byte
		binary.BigEndian.PutUint64(b[:], uint64(n))
		header = append(header, b[:]...)
	}
	header = append(header, mask...)
	masked := make([]byte, n)
	for i := range payload {
		masked[i] = payload[i] ^ mask[i%4]
	}
	if _, err := w.conn.Write(header); err != nil {
		return err
	}
	_, err := w.conn.Write(masked)
	return err
}

func (w *wsClient) WriteText(s string) error { return w.writeFrame(0x1, []byte(s)) }

func (w *wsClient) ReadText() (string, error) {
	var out []byte
	started := false
	for {
		h := make([]byte, 2)
		if _, err := io.ReadFull(w.br, h); err != nil {
			return "", err
		}
		fin := h[0]&0x80 != 0
		opcode := h[0] & 0x0f
		masked := h[1]&0x80 != 0
		n := uint64(h[1] & 0x7f)
		switch n {
		case 126:
			var b [2]byte
			if _, err := io.ReadFull(w.br, b[:]); err != nil {
				return "", err
			}
			n = uint64(binary.BigEndian.Uint16(b[:]))
		case 127:
			var b [8]byte
			if _, err := io.ReadFull(w.br, b[:]); err != nil {
				return "", err
			}
			n = binary.BigEndian.Uint64(b[:])
		}
		if n > 8<<20 {
			return "", errors.New("CDP websocket frame grande demais")
		}
		var mask [4]byte
		if masked {
			if _, err := io.ReadFull(w.br, mask[:]); err != nil {
				return "", err
			}
		}
		payload := make([]byte, int(n))
		if _, err := io.ReadFull(w.br, payload); err != nil {
			return "", err
		}
		if masked {
			for i := range payload {
				payload[i] ^= mask[i%4]
			}
		}
		switch opcode {
		case 0x8:
			return "", io.EOF
		case 0x9:
			_ = w.writeFrame(0xA, payload)
			continue
		case 0xA:
			continue
		case 0x1:
			out = append(out, payload...)
			started = true
		case 0x0:
			if started {
				out = append(out, payload...)
			}
		default:
			continue
		}
		if fin && started {
			return string(out), nil
		}
	}
}

type cdpEnvelope struct {
	ID     int             `json:"id,omitempty"`
	Method string          `json:"method,omitempty"`
	Params json.RawMessage `json:"params,omitempty"`
	Result json.RawMessage `json:"result,omitempty"`
	Error  *struct {
		Message string `json:"message"`
	} `json:"error,omitempty"`
}

func cdpCommand(ws *wsClient, id int, method string, params any) error {
	msg := map[string]any{"id": id, "method": method}
	if params != nil {
		msg["params"] = params
	}
	data, _ := json.Marshal(msg)
	if err := ws.WriteText(string(data)); err != nil {
		return err
	}
	for {
		raw, err := ws.ReadText()
		if err != nil {
			return err
		}
		var env cdpEnvelope
		if json.Unmarshal([]byte(raw), &env) != nil {
			continue
		}
		if env.ID == id {
			if env.Error != nil {
				return errors.New(env.Error.Message)
			}
			return nil
		}
	}
}

func targetHasDiscordWebpack(t cdpTarget) (bool, error) {
	ws, err := dialWS(t.WebSocketDebuggerURL, 2*time.Second)
	if err != nil {
		return false, err
	}
	defer ws.Close()
	id := 1
	msg := map[string]any{
		"id":     id,
		"method": "Runtime.evaluate",
		"params": map[string]any{
			"expression":    `typeof webpackChunkdiscord_app !== "undefined"`,
			"returnByValue": true,
		},
	}
	data, _ := json.Marshal(msg)
	if err := ws.WriteText(string(data)); err != nil {
		return false, err
	}
	for {
		raw, err := ws.ReadText()
		if err != nil {
			return false, err
		}
		var env struct {
			ID     int `json:"id"`
			Result struct {
				Result struct {
					Value bool `json:"value"`
				} `json:"result"`
			} `json:"result"`
		}
		if json.Unmarshal([]byte(raw), &env) == nil && env.ID == id {
			return env.Result.Result.Value, nil
		}
	}
}

func watcherScript(binding string) string {
	// Only observes Discord stores/events. It does not modify experiments, stores or requests.
	return fmt.Sprintf(`(() => {
  const SIGNAL = %q;
  const emit = obj => { try { globalThis[SIGNAL](JSON.stringify(obj)); } catch {} };
  if (globalThis.__goliveStandaloneWatcher) {
    emit({type:"watcher-ready", reused:true});
    return {installed:true,reused:true};
  }
  globalThis.__goliveStandaloneWatcher = true;

  let wreq;
  try {
    webpackChunkdiscord_app.push([[Symbol()], {}, r => { wreq = r; }]);
    webpackChunkdiscord_app.pop();
  } catch (e) {
    emit({type:"watcher-error", error:"webpack-runtime"});
    return {installed:false,error:String(e)};
  }
  if (!wreq) {
    emit({type:"watcher-error", error:"webpack-not-found"});
    return {installed:false};
  }

  const shallowValues = root => {
    const out = [];
    if (root == null) return out;
    out.push(root);
    if (typeof root === "object" || typeof root === "function") {
      try { out.push(...Object.values(root)); } catch {}
    }
    return out.filter(Boolean);
  };
  const allLoaded = () => Object.values(wreq.c || {}).flatMap(m => {
    try { return shallowValues(m?.exports); } catch { return []; }
  });

  let media = null;
  let features = null;
  try { media = wreq(626822)?.Ay ?? null; } catch {}
  try { features = wreq(731854)?.O5 ?? null; } catch {}
  if (!media) media = allLoaded().find(x => typeof x?.supportsInApp === "function" && (x?.constructor?.displayName === "MediaEngineStore" || typeof x?.isVideoEnabled === "function"));
  if (!features) features = allLoaded().find(x => x && Object.prototype.hasOwnProperty.call(x,"VIDEO") && Object.prototype.hasOwnProperty.call(x,"DESKTOP_CAPTURE"));

  const dispatcher = allLoaded().find(x => typeof x?.subscribe === "function" && typeof x?.unsubscribe === "function" && typeof x?.dispatch === "function");
  const voiceStore = allLoaded().find(x => typeof x?.getVoiceChannelId === "function");

  if (!media || !features) {
    emit({type:"watcher-error", error:"media-or-features-not-found", media:!!media, features:!!features});
    return {installed:false, media:!!media, features:!!features};
  }

  let voiceSeen = false;
  let signaled = false;
  const inVoice = () => {
    try { return !!voiceStore?.getVoiceChannelId?.(); } catch { return false; }
  };
  const check = reason => {
    try {
      const storeSaysInVoice = inVoice();
      if (storeSaysInVoice) voiceSeen = true;
      const video = !!media.supportsInApp(features.VIDEO);
      const desktopCapture = !!media.supportsInApp(features.DESKTOP_CAPTURE);
      const voiceReady = voiceStore ? storeSaysInVoice : voiceSeen;
      if (voiceReady && video && desktopCapture && !signaled) {
        signaled = true;
        emit({type:"video-enabled", reason, video, desktopCapture});
      }
    } catch (e) {
      emit({type:"watcher-error", error:"capability-check", detail:String(e)});
    }
  };
  const onVoice = () => {
    voiceSeen = true;
    setTimeout(() => check("VOICE_STATE_UPDATE"), 0);
    setTimeout(() => check("VOICE_STATE_UPDATES"), 150);
    setTimeout(() => check("VOICE_STATE_UPDATES"), 500);
    setTimeout(() => check("VOICE_STATE_UPDATES"), 1200);
  };
  if (dispatcher) {
    try { dispatcher.subscribe("VOICE_STATE_UPDATE", onVoice); } catch {}
    try { dispatcher.subscribe("VOICE_STATE_UPDATES", onVoice); } catch {}
  }
  const timer = setInterval(() => {
    if (signaled) { clearInterval(timer); return; }
    check("poll");
  }, 500);
  setTimeout(() => {
    emit({type:"watcher-ready", dispatcher:!!dispatcher, voiceStore:!!voiceStore});
    check("initial");
  }, 0);
  return {installed:true, dispatcher:!!dispatcher, voiceStore:!!voiceStore};
})()`, binding)
}

type watcherEvent struct {
	Type           string `json:"type"`
	Reason         string `json:"reason,omitempty"`
	Error          string `json:"error,omitempty"`
	Detail         string `json:"detail,omitempty"`
	Dispatcher     bool   `json:"dispatcher,omitempty"`
	VoiceStore     bool   `json:"voiceStore,omitempty"`
	Video          bool   `json:"video,omitempty"`
	DesktopCapture bool   `json:"desktopCapture,omitempty"`
}

func watchGoLiveGate(target cdpTarget, timeout time.Duration, logf func(string, ...any)) error {
	const binding = "__goliveStandaloneSignal"
	ws, err := dialWS(target.WebSocketDebuggerURL, 3*time.Second)
	if err != nil {
		return err
	}
	defer ws.Close()

	cmds := []struct {
		method string
		params any
	}{
		{"Runtime.enable", nil},
		{"Runtime.addBinding", map[string]any{"name": binding}},
		{"Runtime.evaluate", map[string]any{"expression": watcherScript(binding), "returnByValue": true, "awaitPromise": false}},
	}
	for i, c := range cmds {
		if err := cdpCommand(ws, i+1, c.method, c.params); err != nil {
			return fmt.Errorf("CDP %s: %w", c.method, err)
		}
	}
	logf("[hook] Observador instalado no renderer do Discord.")

	_ = ws.conn.SetReadDeadline(time.Now().Add(timeout))
	for {
		raw, err := ws.ReadText()
		if err != nil {
			return err
		}
		var env cdpEnvelope
		if json.Unmarshal([]byte(raw), &env) != nil || env.Method != "Runtime.bindingCalled" {
			continue
		}
		var p struct {
			Name    string `json:"name"`
			Payload string `json:"payload"`
		}
		if json.Unmarshal(env.Params, &p) != nil || p.Name != binding {
			continue
		}
		var ev watcherEvent
		if json.Unmarshal([]byte(p.Payload), &ev) != nil {
			continue
		}
		switch ev.Type {
		case "watcher-ready":
			logf("[hook] Pronto (dispatcher=%v, voiceStore=%v).", ev.Dispatcher, ev.VoiceStore)
		case "watcher-error":
			logf("[hook] Aviso: %s %s", ev.Error, ev.Detail)
		case "video-enabled":
			logf("[hook] Gate liberado após %s (VIDEO=%v, DESKTOP_CAPTURE=%v).", ev.Reason, ev.Video, ev.DesktopCapture)
			return nil
		}
	}
}
