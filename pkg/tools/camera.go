package tools

import (
	"context"
	"crypto/sha1"
	"encoding/base64"
	"encoding/xml"
	"fmt"
	"math/rand"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/sipeed/picoclaw/pkg/config"
	"github.com/sipeed/picoclaw/pkg/logger"
	"github.com/sipeed/picoclaw/pkg/media"
)

// ---------- shared helpers ----------

type cameraEntry struct {
	name     string
	url      string // RTSP URL
	onvifURL string // ONVIF service URL (e.g. http://host:2020/onvif/service)
	user     string
	pass     string
}

func parseCameras(cameras []config.CameraConfig) []cameraEntry {
	entries := make([]cameraEntry, 0, len(cameras))
	for _, c := range cameras {
		e := cameraEntry{
			name: strings.ToLower(c.Name),
			url:  c.URL,
		}
		// Extract user:pass from RTSP URL for ONVIF auth
		// rtsp://user:pass@host:port/path
		if idx := strings.Index(c.URL, "://"); idx >= 0 {
			rest := c.URL[idx+3:]
			if atIdx := strings.Index(rest, "@"); atIdx >= 0 {
				creds := rest[:atIdx]
				hostPort := rest[atIdx+1:]
				if colonIdx := strings.Index(creds, ":"); colonIdx >= 0 {
					e.user = creds[:colonIdx]
					e.pass = creds[colonIdx+1:]
				}
				// Extract host for ONVIF (port 2020)
				host := hostPort
				if slashIdx := strings.Index(host, "/"); slashIdx >= 0 {
					host = host[:slashIdx]
				}
				if colonIdx := strings.Index(host, ":"); colonIdx >= 0 {
					host = host[:colonIdx]
				}
				e.onvifURL = fmt.Sprintf("http://%s:2020/onvif/service", host)
			}
		}
		entries = append(entries, e)
	}
	return entries
}

func cameraMap(entries []cameraEntry) map[string]*cameraEntry {
	m := make(map[string]*cameraEntry, len(entries))
	for i := range entries {
		m[entries[i].name] = &entries[i]
	}
	return m
}

func cameraNames(entries []cameraEntry) []string {
	names := make([]string, len(entries))
	for i, e := range entries {
		names[i] = e.name
	}
	return names
}

// captureFrame grabs a single JPEG frame from an RTSP URL.
// Downscales to 800px width and uses moderate quality for fast transfer.
func captureFrame(ctx context.Context, rtspURL string) ([]byte, error) {
	tmpPath := filepath.Join(os.TempDir(), fmt.Sprintf("cam_%d.jpg", time.Now().UnixNano()))
	defer os.Remove(tmpPath)

	cmdCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	cmd := exec.CommandContext(cmdCtx, "ffmpeg",
		"-rtsp_transport", "tcp",
		"-i", rtspURL,
		"-frames:v", "1",
		"-update", "1",
		"-vf", "scale=800:-1",
		"-q:v", "5",
		"-y",
		tmpPath,
	)
	output, err := cmd.CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("ffmpeg: %v (%s)", err, strings.TrimSpace(string(output)))
	}

	return os.ReadFile(tmpPath)
}

// ---------- CameraSnapshotTool (LLM analysis) ----------

type CameraSnapshotTool struct {
	entries []cameraEntry
	byName  map[string]*cameraEntry
}

func NewCameraSnapshotTool(cameras []config.CameraConfig) *CameraSnapshotTool {
	entries := parseCameras(cameras)
	return &CameraSnapshotTool{entries: entries, byName: cameraMap(entries)}
}

func (t *CameraSnapshotTool) Name() string { return "camera_snapshot" }
func (t *CameraSnapshotTool) Description() string {
	return fmt.Sprintf(
		"Capture a snapshot from a camera and return it for visual analysis by the LLM. "+
			"Available: %s, or \"all\". Use this when user asks what's happening on camera.",
		strings.Join(cameraNames(t.entries), ", "))
}
func (t *CameraSnapshotTool) Parameters() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"camera": map[string]any{
				"type":        "string",
				"description": fmt.Sprintf("Camera name (%s) or \"all\"", strings.Join(cameraNames(t.entries), ", ")),
			},
		},
	}
}

func (t *CameraSnapshotTool) Execute(ctx context.Context, args map[string]any) *ToolResult {
	cam := strings.ToLower(strings.TrimSpace(args["camera"].(string)))
	if cam == "" && len(t.entries) == 1 {
		cam = t.entries[0].name
	}
	if cam == "" {
		return ErrorResult("camera required. Available: " + strings.Join(cameraNames(t.entries), ", "))
	}
	if cam == "all" || cam == "все" {
		return t.captureAll(ctx)
	}
	e, ok := t.byName[cam]
	if !ok {
		return ErrorResult(fmt.Sprintf("unknown camera %q", cam))
	}
	data, err := captureFrame(ctx, e.url)
	if err != nil {
		return ErrorResult(fmt.Sprintf("camera %q: %v", cam, err))
	}
	dataURL := "data:image/jpeg;base64," + base64.StdEncoding.EncodeToString(data)
	return &ToolResult{
		ForLLM:  fmt.Sprintf("[Snapshot from %q (%d KB). Describe what you see.]", cam, len(data)/1024),
		ForUser: fmt.Sprintf("📷 %s", cam),
		Media:   []string{dataURL},
	}
}

func (t *CameraSnapshotTool) captureAll(ctx context.Context) *ToolResult {
	type res struct {
		name string
		data []byte
		err  error
	}
	results := make([]res, len(t.entries))
	var wg sync.WaitGroup
	for i, e := range t.entries {
		wg.Add(1)
		go func(idx int, entry cameraEntry) {
			defer wg.Done()
			d, err := captureFrame(ctx, entry.url)
			results[idx] = res{entry.name, d, err}
		}(i, e)
	}
	wg.Wait()

	var descs []string
	var mediaURLs []string
	for _, r := range results {
		if r.err != nil {
			descs = append(descs, fmt.Sprintf("[%s: error — %v]", r.name, r.err))
		} else {
			mediaURLs = append(mediaURLs, "data:image/jpeg;base64,"+base64.StdEncoding.EncodeToString(r.data))
			descs = append(descs, fmt.Sprintf("[%s: %d KB]", r.name, len(r.data)/1024))
		}
	}
	return &ToolResult{
		ForLLM:  strings.Join(descs, "\n") + "\nImages attached in order. Describe each camera.",
		ForUser: "📷 Все камеры",
		Media:   mediaURLs,
	}
}

// ---------- CameraSendPhotoTool (send photo to user) ----------

type CameraSendPhotoTool struct {
	entries    []cameraEntry
	byName     map[string]*cameraEntry
	mediaStore media.MediaStore
}

func NewCameraSendPhotoTool(cameras []config.CameraConfig, store media.MediaStore) *CameraSendPhotoTool {
	entries := parseCameras(cameras)
	return &CameraSendPhotoTool{entries: entries, byName: cameraMap(entries), mediaStore: store}
}

// SetMediaStore injects the media store (called after creation by AgentLoop).
func (t *CameraSendPhotoTool) SetMediaStore(store media.MediaStore) {
	t.mediaStore = store
}

func (t *CameraSendPhotoTool) Name() string { return "camera_send_photo" }
func (t *CameraSendPhotoTool) Description() string {
	return fmt.Sprintf(
		"Capture a photo from a camera and SEND it to the user (not for LLM analysis, but to deliver the actual image). "+
			"Available: %s, or \"all\".",
		strings.Join(cameraNames(t.entries), ", "))
}
func (t *CameraSendPhotoTool) Parameters() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"camera": map[string]any{
				"type":        "string",
				"description": fmt.Sprintf("Camera name (%s) or \"all\"", strings.Join(cameraNames(t.entries), ", ")),
			},
		},
	}
}

func (t *CameraSendPhotoTool) Execute(ctx context.Context, args map[string]any) *ToolResult {
	cam := strings.ToLower(strings.TrimSpace(args["camera"].(string)))
	if cam == "" && len(t.entries) == 1 {
		cam = t.entries[0].name
	}
	if cam == "" {
		return ErrorResult("camera required")
	}

	names := cameraNames(t.entries)
	var targets []*cameraEntry
	if cam == "all" || cam == "все" {
		for i := range t.entries {
			targets = append(targets, &t.entries[i])
		}
	} else {
		e, ok := t.byName[cam]
		if !ok {
			return ErrorResult(fmt.Sprintf("unknown camera %q. Available: %s", cam, strings.Join(names, ", ")))
		}
		targets = []*cameraEntry{e}
	}

	var refs []string
	for _, e := range targets {
		data, err := captureFrame(ctx, e.url)
		if err != nil {
			logger.WarnCF("camera", "Failed to capture for send", map[string]any{"camera": e.name, "error": err})
			continue
		}
		// Save to temp file and store in media store
		tmpPath := filepath.Join(os.TempDir(), fmt.Sprintf("cam_%s_%d.jpg", e.name, time.Now().UnixNano()))
		if err := os.WriteFile(tmpPath, data, 0644); err != nil {
			continue
		}
		channel := ToolChannel(ctx)
		chatID := ToolChatID(ctx)
		scope := fmt.Sprintf("tool:camera:%s:%s", channel, chatID)
		ref, err := t.mediaStore.Store(tmpPath, media.MediaMeta{
			Filename:    fmt.Sprintf("%s.jpg", e.name),
			ContentType: "image/jpeg",
			Source:      "tool:camera_send_photo",
		}, scope)
		// Don't remove tmpPath — media store holds a reference to it;
		// cleanup happens via media store's ReleaseAll / CleanExpired.
		if err != nil {
			logger.WarnCF("camera", "Failed to store media", map[string]any{"camera": e.name, "error": err})
			continue
		}
		refs = append(refs, ref)
	}

	if len(refs) == 0 {
		return ErrorResult("failed to capture any photos")
	}

	return MediaResult(
		fmt.Sprintf("Sent %d photo(s) to user", len(refs)),
		refs,
	)
}

// ---------- CameraMoveTool (PTZ via ONVIF) ----------

type CameraMoveTool struct {
	entries []cameraEntry
	byName  map[string]*cameraEntry
}

func NewCameraMoveTool(cameras []config.CameraConfig) *CameraMoveTool {
	entries := parseCameras(cameras)
	return &CameraMoveTool{entries: entries, byName: cameraMap(entries)}
}

func (t *CameraMoveTool) Name() string { return "camera_move" }
func (t *CameraMoveTool) Description() string {
	return fmt.Sprintf(
		"Move/rotate a PTZ camera. Directions: up, down, left, right, home (reset to default position). "+
			"Available cameras: %s.",
		strings.Join(cameraNames(t.entries), ", "))
}
func (t *CameraMoveTool) Parameters() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"camera": map[string]any{
				"type":        "string",
				"description": "Camera name",
			},
			"direction": map[string]any{
				"type":        "string",
				"enum":        []string{"up", "down", "left", "right", "home"},
				"description": "Direction to move the camera",
			},
			"speed": map[string]any{
				"type":        "number",
				"description": "Movement speed 0.0-1.0 (default 0.5)",
			},
		},
		"required": []string{"camera", "direction"},
	}
}

func (t *CameraMoveTool) Execute(ctx context.Context, args map[string]any) *ToolResult {
	cam := strings.ToLower(strings.TrimSpace(args["camera"].(string)))
	direction, _ := args["direction"].(string)
	direction = strings.ToLower(strings.TrimSpace(direction))

	speed := 0.5
	if s, ok := args["speed"].(float64); ok && s > 0 && s <= 1 {
		speed = s
	}

	e, ok := t.byName[cam]
	if !ok {
		return ErrorResult(fmt.Sprintf("unknown camera %q", cam))
	}
	if e.onvifURL == "" {
		return ErrorResult(fmt.Sprintf("camera %q has no ONVIF endpoint", cam))
	}

	if direction == "home" {
		err := onvifGotoHome(ctx, e.onvifURL, e.user, e.pass)
		if err != nil {
			return ErrorResult(fmt.Sprintf("PTZ home failed: %v", err))
		}
		return SilentResult(fmt.Sprintf("Camera %q moved to home position", cam))
	}

	var panX, tiltY float64
	switch direction {
	case "left":
		panX = -speed
	case "right":
		panX = speed
	case "up":
		tiltY = speed
	case "down":
		tiltY = -speed
	default:
		return ErrorResult("direction must be: up, down, left, right, home")
	}

	err := onvifContinuousMove(ctx, e.onvifURL, e.user, e.pass, panX, tiltY)
	if err != nil {
		return ErrorResult(fmt.Sprintf("PTZ move failed: %v", err))
	}

	// Move for a short duration then stop
	time.Sleep(500 * time.Millisecond)
	_ = onvifStop(ctx, e.onvifURL, e.user, e.pass)

	return SilentResult(fmt.Sprintf("Camera %q moved %s", cam, direction))
}

// ---------- ONVIF PTZ helpers ----------

func onvifContinuousMove(ctx context.Context, serviceURL, user, pass string, panX, tiltY float64) error {
	body := fmt.Sprintf(`<ContinuousMove xmlns="http://www.onvif.org/ver20/ptz/wsdl">
		<ProfileToken>profile_1</ProfileToken>
		<Velocity><PanTilt x="%.2f" y="%.2f" xmlns="http://www.onvif.org/ver10/schema"/></Velocity>
	</ContinuousMove>`, panX, tiltY)
	_, err := onvifCall(ctx, serviceURL, user, pass, body)
	return err
}

func onvifStop(ctx context.Context, serviceURL, user, pass string) error {
	body := `<Stop xmlns="http://www.onvif.org/ver20/ptz/wsdl">
		<ProfileToken>profile_1</ProfileToken>
		<PanTilt>true</PanTilt><Zoom>true</Zoom>
	</Stop>`
	_, err := onvifCall(ctx, serviceURL, user, pass, body)
	return err
}

func onvifGotoHome(ctx context.Context, serviceURL, user, pass string) error {
	body := `<GotoHomePosition xmlns="http://www.onvif.org/ver20/ptz/wsdl">
		<ProfileToken>profile_1</ProfileToken>
	</GotoHomePosition>`
	_, err := onvifCall(ctx, serviceURL, user, pass, body)
	return err
}

func onvifCall(ctx context.Context, serviceURL, user, pass, soapBody string) ([]byte, error) {
	// WS-Security UsernameToken with PasswordDigest
	nonce := make([]byte, 16)
	rand.Read(nonce)
	created := time.Now().UTC().Format(time.RFC3339Nano)
	nonceB64 := base64.StdEncoding.EncodeToString(nonce)

	// PasswordDigest = Base64(SHA1(nonce + created + password))
	h := sha1.New()
	h.Write(nonce)
	h.Write([]byte(created))
	h.Write([]byte(pass))
	digest := base64.StdEncoding.EncodeToString(h.Sum(nil))

	envelope := fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8"?>
<s:Envelope xmlns:s="http://www.w3.org/2003/05/soap-envelope"
  xmlns:wsse="http://docs.oasis-open.org/wss/2004/01/oasis-200401-wss-wssecurity-secext-1.0.xsd"
  xmlns:wsu="http://docs.oasis-open.org/wss/2004/01/oasis-200401-wss-wssecurity-utility-1.0.xsd">
<s:Header>
  <wsse:Security s:mustUnderstand="true">
    <wsse:UsernameToken>
      <wsse:Username>%s</wsse:Username>
      <wsse:Password Type="http://docs.oasis-open.org/wss/2004/01/oasis-200401-wss-username-token-profile-1.0#PasswordDigest">%s</wsse:Password>
      <wsse:Nonce EncodingType="http://docs.oasis-open.org/wss/2004/01/oasis-200401-wss-soap-message-security-1.0#Base64Binary">%s</wsse:Nonce>
      <wsu:Created>%s</wsu:Created>
    </wsse:UsernameToken>
  </wsse:Security>
</s:Header>
<s:Body>%s</s:Body>
</s:Envelope>`, user, digest, nonceB64, created, soapBody)

	reqCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(reqCtx, "POST", serviceURL, strings.NewReader(envelope))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/soap+xml; charset=utf-8")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	var result struct {
		XMLName xml.Name
		Body    struct {
			Fault *struct {
				Reason struct {
					Text string `xml:"Text"`
				} `xml:"Reason"`
			} `xml:"Fault"`
		} `xml:"Body"`
	}

	body := make([]byte, 8192)
	n, _ := resp.Body.Read(body)
	body = body[:n]

	if resp.StatusCode != 200 {
		return body, fmt.Errorf("ONVIF HTTP %d: %s", resp.StatusCode, string(body))
	}

	if err := xml.Unmarshal(body, &result); err == nil && result.Body.Fault != nil {
		return body, fmt.Errorf("ONVIF fault: %s", result.Body.Fault.Reason.Text)
	}

	return body, nil
}
