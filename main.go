package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	mqtt "github.com/eclipse/paho.mqtt.golang"
)

// ------------------------------------------------------------
// README-style summary (see README.md for full details)
//
// - Connects to a Bambu Lab printer's MQTT broker over TLS (port 8883).
// - Subscribes to device/<UUID>/report (UUID can be auto-discovered).
// - Parses print.gcode_state (RUNNING, FINISH, FAILED).
// - On RUNNING: starts ffmpeg to remux RTSP to MP4 (no re-encode).
// - On FINISH/FAILED: sends 'q' to ffmpeg to finalize the MP4 cleanly.
// - Clean shutdown on SIGINT/SIGTERM; waits STOP_GRACE_SECONDS before escalating.
// - Injects MQTT credentials into RTSP URL for auth; filename uses subtask_name.
// ------------------------------------------------------------

// =========================
// Configuration
// =========================

type Config struct {
	PrinterHost     string
	MQTTPort        int
	DeviceUUID      string
	MQTTUsername    string
	MQTTPassword    string
	MQTTInsecureTLS bool
	MQTTCAFile      string

	OutputDir       string
	FFmpegPath      string
	StopGrace       time.Duration
	EnableFastStart bool
	ExtraArgs       []string
	Location        *time.Location
}

func getenvTrim(key, def string) string {
	val := strings.TrimSpace(os.Getenv(key))
	if val == "" {
		return def
	}
	return val
}

func mustLoadConfig() Config {
	port := 8883
	if s := strings.TrimSpace(os.Getenv("MQTT_PORT")); s != "" {
		if n, err := strconv.Atoi(s); err == nil && n > 0 {
			port = n
		}
	}
	stopGrace := 120 * time.Second
	if s := strings.TrimSpace(os.Getenv("STOP_GRACE_SECONDS")); s != "" {
		if n, err := strconv.Atoi(s); err == nil && n > 0 {
			stopGrace = time.Duration(n) * time.Second
		} else {
			log.Printf("WARN: invalid STOP_GRACE_SECONDS=%q, using default %v", s, stopGrace)
		}
	}

	enableFastStart := false
	if b := strings.TrimSpace(strings.ToLower(os.Getenv("FFMPEG_FASTSTART"))); b != "" {
		enableFastStart = (b == "1" || b == "true" || b == "yes" || b == "on")
	}

	insecureTLS := false
	if b := strings.TrimSpace(strings.ToLower(os.Getenv("MQTT_INSECURE_TLS"))); b != "" {
		insecureTLS = (b == "1" || b == "true" || b == "yes" || b == "on")
	}

	loc := time.Local
	if tz := strings.TrimSpace(os.Getenv("TZ")); tz != "" {
		if l, err := time.LoadLocation(tz); err == nil {
			loc = l
		} else {
			log.Printf("WARN: could not load TZ %q: %v; using local time", tz, err)
		}
	}

	cfg := Config{
		PrinterHost:     getenvTrim("PRINTER_HOST", ""),
		MQTTPort:        port,
		DeviceUUID:      getenvTrim("DEVICE_UUID", ""),
		MQTTUsername:    getenvTrim("MQTT_USERNAME", "bblp"),
		MQTTPassword:    getenvTrim("MQTT_PASSWORD", ""),
		MQTTInsecureTLS: insecureTLS,
		MQTTCAFile:      getenvTrim("MQTT_CA_FILE", ""),
		OutputDir:       getenvTrim("OUTPUT_DIR", "./recordings"),
		FFmpegPath:      getenvTrim("FFMPEG_PATH", "ffmpeg"),
		StopGrace:       stopGrace,
		EnableFastStart: enableFastStart,
		ExtraArgs:       fieldsAllowQuotes(getenvTrim("FFMPEG_EXTRA_ARGS", "")),
		Location:        loc,
	}

	// DEVICE_UUID is optional (auto-discovered if empty).
	if cfg.PrinterHost == "" || cfg.MQTTPassword == "" {
		log.Fatalf("Missing required env vars. Need PRINTER_HOST and MQTT_PASSWORD. (MQTT_USERNAME defaults to 'bblp'; DEVICE_UUID will be auto-discovered if omitted).")
	}

	if err := os.MkdirAll(cfg.OutputDir, 0o755); err != nil {
		log.Fatalf("Failed to create OUTPUT_DIR %q: %v", cfg.OutputDir, err)
	}

	log.Printf("config: host=%s port=%d device=%s faststart=%v stopGrace=%v",
		cfg.PrinterHost, cfg.MQTTPort, cfg.DeviceUUID, cfg.EnableFastStart, cfg.StopGrace)

	return cfg
}

// fieldsAllowQuotes splits a string on spaces, keeping quoted segments intact (no escape handling).
func fieldsAllowQuotes(s string) []string {
	if s == "" {
		return nil
	}
	var out []string
	var cur strings.Builder
	inQ := false
	for _, r := range s {
		switch r {
		case '"':
			inQ = !inQ
		case ' ':
			if inQ {
				cur.WriteRune(r)
			} else if cur.Len() > 0 {
				out = append(out, cur.String())
				cur.Reset()
			}
		default:
			cur.WriteRune(r)
		}
	}
	if cur.Len() > 0 {
		out = append(out, cur.String())
	}
	return out
}

// =========================
// MQTT payload shape
// =========================

type reportPayload struct {
	Print struct {
		GCodeState  string `json:"gcode_state"`
		SubtaskName string `json:"subtask_name"`
		IPCam       struct {
			RTSPURL string `json:"rtsp_url"`
		} `json:"ipcam"`
	} `json:"print"`
}

// =========================
// Filename helpers
// =========================

var invalidChars = regexp.MustCompile(`[^a-zA-Z0-9._-]+`)

func sanitizeName(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return "unnamed"
	}
	s = invalidChars.ReplaceAllString(s, "_")
	s = regexp.MustCompile(`_+`).ReplaceAllString(s, "_")
	s = strings.Trim(s, "._-")
	if s == "" {
		return "unnamed"
	}
	if len(s) > 80 {
		s = s[:80]
	}
	return s
}

func buildOutputFilename(now time.Time, name string) string {
	return fmt.Sprintf("%s-%s.mp4", now.Format("2006-01-02-150405"), sanitizeName(name))
}

func nextNonClobberingPath(dir, base string) (string, error) {
	out := filepath.Join(dir, base)
	if _, err := os.Stat(out); errors.Is(err, os.ErrNotExist) {
		return out, nil
	}
	ext := filepath.Ext(base)
	name := strings.TrimSuffix(base, ext)
	for i := 1; i < 10000; i++ {
		candidate := filepath.Join(dir, fmt.Sprintf("%s-%d%s", name, i, ext))
		if _, err := os.Stat(candidate); errors.Is(err, os.ErrNotExist) {
			return candidate, nil
		}
	}
	return "", fmt.Errorf("unable to create unique filename for %q", base)
}

// =========================
// FFmpeg recorder
// =========================

type Recorder interface {
	Start(ctx context.Context, rtspURL, outputPath string) error
	Stop(ctx context.Context) error
	Running() bool
}

type FFmpegRecorder struct {
	mu        sync.Mutex
	cmd       *exec.Cmd
	stdin     io.WriteCloser
	path      string
	extraArgs []string
	faststart bool
	outPath   string
}

func NewFFmpegRecorder(ffmpegPath string, faststart bool, extraArgs []string) *FFmpegRecorder {
	return &FFmpegRecorder{
		path:      ffmpegPath,
		extraArgs: extraArgs,
		faststart: faststart,
	}
}

func (r *FFmpegRecorder) Start(ctx context.Context, rtspURL, outputPath string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.cmd != nil {
		return errors.New("recorder already running")
	}

	args := []string{
		"-hide_banner",
		"-nostats",
		"-loglevel", "info",
		"-rtsp_transport", "tcp",
		"-fflags", "+genpts",
		"-use_wallclock_as_timestamps", "1",
		"-i", rtspURL,
		"-c", "copy",
	}
	if r.faststart {
		args = append(args, "-movflags", "+faststart")
	}
	args = append(args, r.extraArgs...)
	args = append(args, outputPath)

	cmd := exec.CommandContext(context.Background(), r.path, args...)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return fmt.Errorf("allocating stdin pipe failed: %w", err)
	}
	r.stdin = stdin
	r.outPath = outputPath
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	if err := cmd.Start(); err != nil {
		return fmt.Errorf("starting ffmpeg failed: %w", err)
	}
	r.cmd = cmd
	log.Printf("[recorder] ffmpeg started (pid=%d), writing to %s", cmd.Process.Pid, outputPath)
	return nil
}

func (r *FFmpegRecorder) Running() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.cmd != nil
}

func (r *FFmpegRecorder) Stop(ctx context.Context) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.cmd == nil {
		return nil
	}

	log.Printf("[recorder] stopping ffmpeg (pid=%d)...", r.cmd.Process.Pid)

	done := make(chan error, 1)
	go func() { done <- r.cmd.Wait() }()

	// Preferred: ask ffmpeg to quit gracefully (writes trailer / faststart move).
	if r.stdin != nil {
		_, _ = io.WriteString(r.stdin, "q\n")
		_ = r.stdin.Close()
	}

	if dl, ok := ctx.Deadline(); ok {
		log.Printf("[recorder] waiting up to %v for ffmpeg to finalize...", time.Until(dl))
	}

	select {
	case err := <-done:
		logFFmpegExit(err)
	case <-ctx.Done():
		// Escalate to SIGINT, then SIGKILL if needed.
		log.Printf("[recorder] WARN: ffmpeg did not exit after 'q'; sending SIGINT...")
		_ = r.cmd.Process.Signal(os.Interrupt)
		select {
		case err := <-done:
			logFFmpegExit(err)
		case <-time.After(10 * time.Second):
			log.Printf("[recorder] WARN: ffmpeg still running; killing...")
			_ = r.cmd.Process.Kill()
			<-done
		}
	}

	if r.outPath != "" {
		if fi, err := os.Stat(r.outPath); err != nil {
			log.Printf("[recorder] WARN: output not found: %s (err=%v)", r.outPath, err)
		} else {
			log.Printf("[recorder] finalized file: %s (%d bytes)", r.outPath, fi.Size())
		}
	}

	r.cmd = nil
	r.stdin = nil
	r.outPath = ""
	return nil
}

func logFFmpegExit(err error) {
	if err == nil {
		log.Printf("[recorder] ffmpeg stopped cleanly (exit=0)")
		return
	}
	if _, ok := err.(*exec.ExitError); ok {
		log.Printf("[recorder] ffmpeg exited (non-zero); likely caller-initiated stop (ok): %v", err)
		return
	}
	log.Printf("[recorder] ffmpeg exited with error: %v", err)
}

// =========================
// TLS / MQTT helpers
// =========================

func buildTLSConfig(cfg Config) (*tls.Config, error) {
	tlsConf := &tls.Config{
		InsecureSkipVerify: cfg.MQTTInsecureTLS,
		MinVersion:         tls.VersionTLS12,
	}
	if cfg.MQTTCAFile != "" {
		data, err := os.ReadFile(cfg.MQTTCAFile)
		if err != nil {
			return nil, fmt.Errorf("reading MQTT_CA_FILE failed: %w", err)
		}
		p := x509.NewCertPool()
		if !p.AppendCertsFromPEM(data) {
			return nil, errors.New("failed to parse MQTT_CA_FILE")
		}
		tlsConf.RootCAs = p
		if cfg.MQTTInsecureTLS {
			log.Printf("WARN: both MQTT_CA_FILE set and MQTT_INSECURE_TLS=true; proceeding INSECURE.")
		}
	}
	return tlsConf, nil
}

func brokerURL(host string, port int) string {
	// Eclipse Paho uses "ssl://" for TLS
	return fmt.Sprintf("ssl://%s:%d", host, port)
}

// Topic / UUID helpers
func uuidFromTopic(topic string) (string, bool) {
	parts := strings.Split(topic, "/")
	if len(parts) >= 3 && parts[0] == "device" && parts[2] == "report" && parts[1] != "" {
		return parts[1], true
	}
	return "", false
}

// RTSP URL auth injection + redacted variant for logs
func injectAuthIntoRTSP(rtsp string, username, password string) (string, string) {
	u, err := url.Parse(rtsp)
	if err != nil {
		return rtsp, rtsp // fallback
	}
	redacted := *u
	if password != "" {
		u.User = url.UserPassword(username, password)
		redacted.User = url.UserPassword(username, "********")
	} else {
		u.User = url.User(username)
		redacted.User = url.User(username)
	}
	return u.String(), redacted.String()
}

// =========================
// App
// =========================

func main() {
	log.SetFlags(log.LstdFlags | log.Lmicroseconds)
	cfg := mustLoadConfig()
	log.Printf("bambu-lan-print-recording starting (Go %s, PID %d)", runtime.Version(), os.Getpid())

	rec := NewFFmpegRecorder(cfg.FFmpegPath, cfg.EnableFastStart, cfg.ExtraArgs)

	// Global app context & signal handling
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	sigCh := make(chan os.Signal, 2)
	signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)
	go func() {
		for {
			s := <-sigCh
			log.Printf("signal received: %v", s)
			stopCtx, stopCancel := context.WithTimeout(context.Background(), cfg.StopGrace)
			_ = rec.Stop(stopCtx)
			stopCancel()
			cancel()
			return
		}
	}()

	tlsConf, err := buildTLSConfig(cfg)
	if err != nil {
		log.Fatalf("TLS config error: %v", err)
	}

	// Subscription state (topic may switch after auto-discovery)
	currentTopic := ""
	if cfg.DeviceUUID == "" {
		currentTopic = "device/+/report"
		log.Printf("[mqtt] DEVICE_UUID not set; using wildcard %q for auto-discovery", currentTopic)
	} else {
		currentTopic = fmt.Sprintf("device/%s/report", cfg.DeviceUUID)
	}
	var subMu sync.Mutex // protects currentTopic and cfg.DeviceUUID
	var mu sync.Mutex    // protects recorder start/stop sequencing

	// Define message handler without self-referencing by using DefaultPublishHandler.
	var messageHandler mqtt.MessageHandler
	messageHandler = func(c mqtt.Client, m mqtt.Message) {
		// Discover UUID from the topic if needed.
		if uuid, ok := uuidFromTopic(m.Topic()); ok {
			if cfg.DeviceUUID == "" {
				subMu.Lock()
				if cfg.DeviceUUID == "" {
					cfg.DeviceUUID = uuid
					oldTopic := currentTopic
					currentTopic = fmt.Sprintf("device/%s/report", uuid)
					subMu.Unlock()
					log.Printf("[mqtt] discovered device uuid: %s (topic=%s)", uuid, m.Topic())
					// Switch subscription; use default handler (nil) to avoid self-ref
					go func() {
						if tok := c.Unsubscribe(oldTopic); tok.Wait() && tok.Error() != nil {
							log.Printf("[mqtt] unsubscribe error from %s: %v", oldTopic, tok.Error())
						}
						if tok := c.Subscribe(currentTopic, 0, nil); tok.Wait() && tok.Error() != nil {
							log.Printf("[mqtt] subscribe error to %s: %v", currentTopic, tok.Error())
						} else {
							log.Printf("[mqtt] switched subscription to %s", currentTopic)
						}
					}()
				} else {
					subMu.Unlock()
				}
			} else if uuid != cfg.DeviceUUID {
				// Ignore messages from other devices (defensive)
				return
			}
		}

		var payload reportPayload
		if err := json.Unmarshal(m.Payload(), &payload); err != nil {
			return
		}
		state := strings.ToUpper(strings.TrimSpace(payload.Print.GCodeState))
		if state == "" {
			return
		}

		switch state {
		case "RUNNING":
			mu.Lock()
			defer mu.Unlock()
			if rec.Running() {
				return // already recording
			}
			rtspBase := strings.TrimSpace(payload.Print.IPCam.RTSPURL)
			if rtspBase == "" {
				log.Printf("[event] RUNNING but ipcam.rtsp_url is empty; cannot start")
				return
			}
			rtspAuth, rtspRedacted := injectAuthIntoRTSP(rtspBase, cfg.MQTTUsername, cfg.MQTTPassword)
			name := payload.Print.SubtaskName
			now := time.Now().In(cfg.Location)
			base := buildOutputFilename(now, name)
			outPath, err := nextNonClobberingPath(cfg.OutputDir, base)
			if err != nil {
				log.Printf("ERROR: cannot allocate output path: %v", err)
				return
			}
			log.Printf("[event] RUNNING → start: %s => %s", rtspRedacted, outPath)
			if err := rec.Start(ctx, rtspAuth, outPath); err != nil {
				log.Printf("ERROR: start recorder: %v", err)
			}

		case "FINISH", "FAILED":
			mu.Lock()
			defer mu.Unlock()
			if !rec.Running() {
				return
			}
			log.Printf("[event] %s → stop", state)
			stopCtx, stopCancel := context.WithTimeout(context.Background(), cfg.StopGrace)
			if err := rec.Stop(stopCtx); err != nil {
				log.Printf("ERROR: stopping recorder: %v", err)
			}
			stopCancel()

		default:
			// Ignore other states like PREPARE/IDLE/etc.
		}
	}

	// Prepare client options with default handler set to messageHandler
	opts := mqtt.NewClientOptions().
		AddBroker(brokerURL(cfg.PrinterHost, cfg.MQTTPort)).
		SetClientID(fmt.Sprintf("bambu-recorder-%d", time.Now().UnixNano())).
		SetUsername(cfg.MQTTUsername).
		SetPassword(cfg.MQTTPassword).
		SetTLSConfig(tlsConf).
		SetCleanSession(true).
		SetAutoReconnect(true).
		SetConnectRetry(true).
		SetConnectRetryInterval(3 * time.Second).
		SetKeepAlive(30 * time.Second).
		SetOrderMatters(false).
		SetDefaultPublishHandler(messageHandler)

	opts.OnConnect = func(c mqtt.Client) {
		log.Printf("[mqtt] connected to %s", brokerURL(cfg.PrinterHost, cfg.MQTTPort))
		// Subscribe to whichever topic is active (wildcard or concrete).
		subMu.Lock()
		t := currentTopic
		subMu.Unlock()
		if tok := c.Subscribe(t, 0, nil); tok.Wait() && tok.Error() != nil {
			log.Printf("[mqtt] subscribe error: %v", tok.Error())
		} else {
			log.Printf("[mqtt] subscribed: %s", t)
		}
	}
	opts.OnConnectionLost = func(c mqtt.Client, err error) {
		log.Printf("[mqtt] connection lost: %v", err)
	}

	client := mqtt.NewClient(opts)
	if tok := client.Connect(); tok.Wait() && tok.Error() != nil {
		log.Fatalf("MQTT connect error: %v", tok.Error())
	}
	defer func() {
		// Clean shutdown: stop any recording, then disconnect MQTT.
		stopCtx, stopCancel := context.WithTimeout(context.Background(), cfg.StopGrace)
		_ = rec.Stop(stopCtx)
		stopCancel()
		client.Disconnect(250)
	}()

	// Block until ctx canceled (signal) or process killed.
	<-ctx.Done()
	log.Printf("exiting")
}
