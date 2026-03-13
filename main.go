package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"os"
	"os/signal"
	"sync"
	"time"

	"github.com/gorilla/websocket"
	"github.com/pion/webrtc/v3"
	"github.com/pion/webrtc/v3/pkg/media"
	"github.com/pion/webrtc/v3/pkg/media/h264reader"
)

type Config struct {
	SignalingURL string `json:"signaling_url"`
	DeviceID     string `json:"device_id"`
}

type Envelope struct {
	Type string          `json:"type"`
	Data json.RawMessage `json:"data"`
}

// PeerInfo mirrors flutter-webrtc-server PeerInfo.
type PeerInfo struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	UserAgent string `json:"user_agent"`
}

type CandidateWrapper struct {
	SDPMLineIndex uint16  `json:"sdpMLineIndex"`
	SDPMid        *string `json:"sdpMid"`
	Candidate     string  `json:"candidate"`
}

// Negotiation structure for offer/answer/candidate routing.
type Negotiation struct {
	From      string             `json:"from"`
	To        string             `json:"to"`
	SessionID string             `json:"session_id"`
	Desc      *webrtc.SessionDescription `json:"description,omitempty"`
	Candidate *CandidateWrapper          `json:"candidate,omitempty"`
}

var (
	pc         *webrtc.PeerConnection
	videoTrack *webrtc.TrackLocalStaticSample
	wsConn     *websocket.Conn
	cfg        Config

	h264Once sync.Once
)

func loadConfig() error {
	f, err := os.Open("config.json")
	if err != nil {
		return err
	}
	defer f.Close()
	return json.NewDecoder(f).Decode(&cfg)
}

func main() {
	if err := loadConfig(); err != nil {
		log.Fatalf("loadConfig failed: %v", err)
	}

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
	defer cancel()

	for {
		if err := connectAndRun(ctx); err != nil {
			log.Printf("connectAndRun error: %v, retrying in 3s...", err)
			select {
			case <-time.After(3 * time.Second):
			case <-ctx.Done():
				return
			}
		} else {
			return
		}
	}
}

func connectAndRun(ctx context.Context) error {
	log.Printf("Connecting to signaling: %s", cfg.SignalingURL)

	var err error
	wsConn, _, err = websocket.DefaultDialer.Dial(cfg.SignalingURL, nil)
	if err != nil {
		return err
	}
	defer wsConn.Close()

	hello := Envelope{
		Type: "new",
		Data: mustJSON(PeerInfo{
			ID:        cfg.DeviceID,
			Name:      "GoCamera-" + cfg.DeviceID,
			UserAgent: "rephone-go-camera/0.1",
		}),
	}
	if err := wsConn.WriteJSON(hello); err != nil {
		return fmt.Errorf("send new failed: %w", err)
	}
	log.Printf("Sent 'new' as camera peer, id=%s", cfg.DeviceID)

	errCh := make(chan error, 1)
	go func() {
		errCh <- readLoop()
	}()

	select {
	case <-ctx.Done():
		return nil
	case err := <-errCh:
		return err
	}
}

func readLoop() error {
	for {
		var env Envelope
		if err := wsConn.ReadJSON(&env); err != nil {
			return err
		}

		switch env.Type {
		case "offer":
			if err := handleOffer(env.Data); err != nil {
				log.Printf("handleOffer error: %v", err)
			}
		case "candidate":
			if err := handleCandidate(env.Data); err != nil {
				log.Printf("handleCandidate error: %v", err)
			}
		case "bye":
			log.Printf("bye received, closing PeerConnection")
			if pc != nil {
				_ = pc.Close()
				pc = nil
				videoTrack = nil
			}
		case "peers", "keepalive":
			// optional logging
		default:
			// ignore unknown for now
		}
	}
}

func handleOffer(data json.RawMessage) error {
	var nego Negotiation
	if err := json.Unmarshal(data, &nego); err != nil {
		return err
	}
	log.Printf("Received offer from=%s session=%s", nego.From, nego.SessionID)

	if pc != nil {
		_ = pc.Close()
		pc = nil
		videoTrack = nil
	}
	if err := createPeerConnection(nego); err != nil {
		return err
	}

	if nego.Desc == nil {
		return fmt.Errorf("offer missing description")
	}
	if err := pc.SetRemoteDescription(*nego.Desc); err != nil {
		return err
	}

	answer, err := pc.CreateAnswer(nil)
	if err != nil {
		return err
	}
	if err := pc.SetLocalDescription(answer); err != nil {
		return err
	}

	resp := Envelope{
		Type: "answer",
		Data: mustJSON(map[string]interface{}{
			"from":       cfg.DeviceID,
			"to":         nego.From,
			"session_id": nego.SessionID,
			"description": map[string]interface{}{
				"sdp":  answer.SDP,
				"type": answer.Type.String(),
			},
		}),
	}
	return wsConn.WriteJSON(resp)
}

func handleCandidate(data json.RawMessage) error {
	var nego Negotiation
	if err := json.Unmarshal(data, &nego); err != nil {
		return err
	}
	if nego.Candidate == nil || pc == nil {
		return nil
	}
	init := webrtc.ICECandidateInit{
		Candidate:     nego.Candidate.Candidate,
		SDPMid:        nego.Candidate.SDPMid,
		SDPMLineIndex: &nego.Candidate.SDPMLineIndex,
	}
	return pc.AddICECandidate(init)
}

func createPeerConnection(nego Negotiation) error {
	config := webrtc.Configuration{
		ICEServers: []webrtc.ICEServer{
			{URLs: []string{"stun:stun.l.google.com:19302"}},
		},
	}
	var err error
	pc, err = webrtc.NewPeerConnection(config)
	if err != nil {
		return err
	}

	videoTrack, err = webrtc.NewTrackLocalStaticSample(
		webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeH264},
		"video", "camera",
	)
	if err != nil {
		return err
	}
	if _, err = pc.AddTrack(videoTrack); err != nil {
		return err
	}
	// Force H264 in answer so we don't negotiate VP8 while sending H264 (would cause black screen).
	for _, tr := range pc.GetTransceivers() {
		if tr.Kind() == webrtc.RTPCodecTypeVideo && tr.Sender() != nil {
			if err := tr.SetCodecPreferences([]webrtc.RTPCodecParameters{
				{RTPCodecCapability: webrtc.RTPCodecCapability{
					MimeType:  webrtc.MimeTypeH264,
					ClockRate: 90000,
				}, PayloadType: 96},
			}); err != nil {
				log.Printf("SetCodecPreferences (H264) warning: %v", err)
			}
			break
		}
	}
	// Start reading real H264 from stdin (ffmpeg pipe) only once per process.
	h264Once.Do(func() {
		go startH264FromStdin()
	})

	pc.OnICECandidate(func(c *webrtc.ICECandidate) {
		if c == nil {
			return
		}
		init := c.ToJSON()
		msg := Envelope{
			Type: "candidate",
			Data: mustJSON(map[string]interface{}{
				"from":       cfg.DeviceID,
				"to":         nego.From,
				"session_id": nego.SessionID,
				"candidate": map[string]interface{}{
					"sdpMLineIndex": init.SDPMLineIndex,
					"sdpMid":        init.SDPMid,
					"candidate":     init.Candidate,
				},
			}),
		}
		if err := wsConn.WriteJSON(msg); err != nil {
			log.Printf("send candidate error: %v", err)
		}
	})

	return nil
}

var firstSampleLogged bool

// findNextStartCode returns the index of the next Annex-B start code (0x000001 or 0x00000001) in data.
// Returns -1 if not found.
func findNextStartCode(data []byte, from int) int {
	for i := from; i < len(data)-2; i++ {
		if data[i] == 0 && data[i+1] == 0 {
			if data[i+2] == 1 {
				return i
			}
			if i+3 < len(data) && data[i+2] == 0 && data[i+3] == 1 {
				return i
			}
		}
	}
	return -1
}

// nalType returns H.264 NAL type (1–5 = VCL slice, 6 = SEI, 7 = SPS, 8 = PPS). data starts at first byte after start code.
func nalType(data []byte, startCodeLen int) int {
	if startCodeLen >= len(data) {
		return 0
	}
	return int(data[startCodeLen] & 0x1f)
}

// isVCL returns true for slice NALs (type 1 = non-IDR, type 5 = IDR) that form a frame.
func isVCL(typ int) bool {
	return typ == 1 || typ == 5
}

// startH264FromStdin uses Pion 的 h264reader 从 stdin 读取 Annex-B H264，
// 完全复用官方 play-from-disk-h264 的 NAL 解析逻辑，避免手写解析带来的花屏问题。
// 该 goroutine 在进程生命周期内只启动一次，通过全局 videoTrack 将数据送入当前活跃的 PeerConnection。
func startH264FromStdin() {
	reader := bufio.NewReader(os.Stdin)
	h264, err := h264reader.NewReader(reader)
	if err != nil {
		log.Printf("h264reader.NewReader error: %v", err)
		return
	}

	frameInterval := time.Second / 30
	for {
		nal, readErr := h264.NextNAL()
		if readErr != nil {
			if readErr == io.EOF {
				log.Printf("startH264FromStdin: EOF, stopping")
			} else {
				log.Printf("startH264FromStdin: NextNAL error: %v", readErr)
			}
			return
		}
		if nal == nil || len(nal.Data) == 0 {
			continue
		}

		// 没有活跃的 track 时，丢弃此帧但保持读取，以免阻塞 ffmpeg 管道。
		currentTrack := videoTrack
		if currentTrack == nil {
			continue
		}

		// h264reader 返回的 Data 不含起始码，按官方示例前面补 0x00000001。
		payload := make([]byte, 4+len(nal.Data))
		copy(payload[0:4], []byte{0x00, 0x00, 0x00, 0x01})
		copy(payload[4:], nal.Data)

		typ := int(nal.Data[0] & 0x1f)
		// 只有 VCL (1/5) 推进时间轴，其余 Duration=0。
		duration := time.Duration(0)
		if isVCL(typ) {
			duration = frameInterval
		}

		if !firstSampleLogged {
			firstSampleLogged = true
			log.Printf("First NAL via h264reader: %d bytes (type %d)", len(payload), typ)
		}

		if writeErr := currentTrack.WriteSample(media.Sample{Data: payload, Duration: duration}); writeErr != nil {
			log.Printf("WriteSample error: %v", writeErr)
			return
		}
	}
}

func mustJSON(v interface{}) json.RawMessage {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return b
}

