package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"os/signal"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
	"github.com/pion/rtp"
	"github.com/pion/webrtc/v4"
	"github.com/pion/webrtc/v4/pkg/media"
	"github.com/pion/webrtc/v4/pkg/media/h264reader"
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
	audioTrack *webrtc.TrackLocalStaticRTP
	wsConn     *websocket.Conn
	cfg        Config

	h264Once  sync.Once
	audioOnce sync.Once
	wsMu      sync.Mutex

	// micEnabled 表示“相机端是否应该发送音频”这一逻辑开关。
	// 目前还没有真正的音频采集与发送逻辑，这个开关主要由 DataChannel 的
	// `{"type":"camera_mic","enabled":true/false}` 消息来控制，方便后续在音频管线中接入。
	micEnabled bool
)

func wsWriteJSON(v interface{}) error {
	wsMu.Lock()
	defer wsMu.Unlock()
	if wsConn == nil {
		return fmt.Errorf("wsConn is nil")
	}
	return wsConn.WriteJSON(v)
}

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
	if err := wsWriteJSON(hello); err != nil {
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
				audioTrack = nil
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
		audioTrack = nil
	}
	audioEnabled := offerSupportsOpusAudio(nego.Desc)
	if err := createPeerConnection(nego, audioEnabled); err != nil {
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
	return wsWriteJSON(resp)
}

func handleCandidate(data json.RawMessage) error {
	var nego Negotiation
	if err := json.Unmarshal(data, &nego); err != nil {
		return err
	}
	if nego.Candidate == nil || pc == nil {
		return nil
	}

	// 打印收到的 candidate
	log.Printf("RECV candidate: from=%s to=%s sid=%s mid=%v mline=%d cand=%s",
		nego.From, nego.To, nego.SessionID,
		nego.Candidate.SDPMid, nego.Candidate.SDPMLineIndex, nego.Candidate.Candidate,
	)
	init := webrtc.ICECandidateInit{
		Candidate:     nego.Candidate.Candidate,
		SDPMid:        nego.Candidate.SDPMid,
		SDPMLineIndex: &nego.Candidate.SDPMLineIndex,
	}
	return pc.AddICECandidate(init)
}

func createPeerConnection(nego Negotiation, enableAudio bool) error {
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

	if enableAudio {
		audioTrack, err = webrtc.NewTrackLocalStaticRTP(
			webrtc.RTPCodecCapability{
				MimeType:  webrtc.MimeTypeOpus,
				ClockRate: 48000,
				// Most WebRTC endpoints negotiate Opus as 2-channel in SDP (opus/48000/2).
				// Using 2 here avoids "codec is not supported by remote" on answer creation.
				Channels:  2,
			},
			"audio", "microphone",
		)
		if err != nil {
			return err
		}
		if _, err = pc.AddTrack(audioTrack); err != nil {
			return err
		}
		log.Printf("Audio track enabled (remote offer supports Opus)")
	} else {
		audioTrack = nil
		log.Printf("Audio track disabled (remote offer has no Opus audio m-line)")
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
	// Read Opus RTP packets from local UDP and forward to WebRTC audio track.
	// Example source:
	// ffmpeg -f alsa -i hw:3,0 -ac 1 -ar 48000 -c:a libopus -application voip -frame_duration 20 -vn -f rtp rtp://127.0.0.1:5006
	if enableAudio {
		audioOnce.Do(func() {
			go startOpusFromUDP("127.0.0.1:5006")
		})
	}

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

		// 打印发送出去的 candidate
		log.Printf("SEND candidate: from=%s to=%s sid=%s mid=%s mline=%d cand=%s",
			cfg.DeviceID, nego.From, nego.SessionID,
			init.SDPMid, init.SDPMLineIndex, init.Candidate,
		)

		if err := wsWriteJSON(msg); err != nil {
			log.Printf("send candidate error: %v", err)
		}
	})

	// 监控端会在其 RTCPeerConnection 上创建 DataChannel（用于事件回放、缩略图、麦克风控制等）。
	// 这里监听 DataChannel 并解析 JSON 消息中的 "camera_mic" 指令，更新本端 micEnabled 状态，
	// 以便后续接入真正的音频采集/发送逻辑时使用。
	pc.OnDataChannel(func(dc *webrtc.DataChannel) {
		log.Printf("DataChannel opened: label=%s id=%d", dc.Label(), dc.ID())

		dc.OnMessage(func(msg webrtc.DataChannelMessage) {
			if msg.IsString {
				var payload map[string]interface{}
				if err := json.Unmarshal(msg.Data, &payload); err == nil {
					if t, ok := payload["type"].(string); ok && t == "camera_mic" {
						enabled, _ := payload["enabled"].(bool)
						micEnabled = enabled
						log.Printf("camera_mic control received via DataChannel, enabled=%v", enabled)
						// TODO: 当接入真实音频采集时，在这里根据 micEnabled 打开/关闭音频发送。
						return
					}
				}
			}
			// 其他类型消息暂时仅打印日志，方便后续扩展。
			log.Printf("DataChannel message (label=%s): %d bytes", dc.Label(), len(msg.Data))
		})
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

// rbspFromNAL removes emulation-prevention bytes (0x03) from NAL payload.
// Input data should start from the first byte AFTER the NAL header.
func rbspFromNAL(data []byte) []byte {
	if len(data) == 0 {
		return nil
	}
	out := make([]byte, 0, len(data))
	zeroCount := 0
	for _, b := range data {
		if zeroCount >= 2 && b == 0x03 {
			// skip emulation-prevention byte
			zeroCount = 0
			continue
		}
		out = append(out, b)
		if b == 0x00 {
			zeroCount++
		} else {
			zeroCount = 0
		}
	}
	return out
}

type bitReader struct {
	b   []byte
	pos int // bit position
}

func (r *bitReader) readBit() (uint8, bool) {
	if r.pos >= len(r.b)*8 {
		return 0, false
	}
	byteIdx := r.pos / 8
	bitIdx := 7 - (r.pos % 8)
	r.pos++
	return (r.b[byteIdx] >> bitIdx) & 1, true
}

func (r *bitReader) readUE() (uint32, bool) {
	// Exp-Golomb ue(v)
	zeros := 0
	for {
		bit, ok := r.readBit()
		if !ok {
			return 0, false
		}
		if bit == 0 {
			zeros++
			// avoid pathological
			if zeros > 31 {
				return 0, false
			}
			continue
		}
		break
	}
	if zeros == 0 {
		return 0, true
	}
	var val uint32
	for i := 0; i < zeros; i++ {
		bit, ok := r.readBit()
		if !ok {
			return 0, false
		}
		val = (val << 1) | uint32(bit)
	}
	return (1<<uint32(zeros) - 1) + val, true
}

// isFirstSlice checks whether this VCL NAL is the first slice of a new frame (first_mb_in_slice == 0).
// nalData is the raw NAL bytes (without Annex-B start code), starting with the NAL header.
func isFirstSlice(nalData []byte) bool {
	if len(nalData) < 2 {
		return false
	}
	typ := int(nalData[0] & 0x1f)
	if typ != 1 && typ != 5 {
		return false
	}
	rbsp := rbspFromNAL(nalData[1:])
	br := &bitReader{b: rbsp}
	firstMB, ok := br.readUE()
	if !ok {
		return false
	}
	return firstMB == 0
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

		currentTrack := videoTrack
		if currentTrack == nil {
			continue
		}

		// h264reader 返回的 Data 不含起始码，前面补 0x00000001。
		payload := make([]byte, 4+len(nal.Data))
		copy(payload[0:4], []byte{0x00, 0x00, 0x00, 0x01})
		copy(payload[4:], nal.Data)

		typ := int(nal.Data[0] & 0x1f)
		duration := time.Duration(0)
		if isVCL(typ) {
			duration = frameInterval
		}

		if !firstSampleLogged {
			firstSampleLogged = true
			log.Printf("First NAL via h264reader: %d bytes (type %d)", len(payload), typ)
		}

		if writeErr := currentTrack.WriteSample(
			media.Sample{Data: payload, Duration: duration},
		); writeErr != nil {
			log.Printf("WriteSample error: %v", writeErr)
			return
		}
	}
}

func startOpusFromUDP(addr string) {
	pcAddr, err := net.ResolveUDPAddr("udp", addr)
	if err != nil {
		log.Printf("startOpusFromUDP resolve addr error: %v", err)
		return
	}
	conn, err := net.ListenUDP("udp", pcAddr)
	if err != nil {
		log.Printf("startOpusFromUDP listen error: %v", err)
		return
	}
	defer conn.Close()
	log.Printf("startOpusFromUDP listening on %s", addr)

	buf := make([]byte, 1600)
	pkt := &rtp.Packet{}
	for {
		n, _, readErr := conn.ReadFromUDP(buf)
		if readErr != nil {
			log.Printf("startOpusFromUDP read error: %v", readErr)
			return
		}
		if !micEnabled {
			continue
		}

		currentTrack := audioTrack
		if currentTrack == nil {
			continue
		}

		if err := pkt.Unmarshal(buf[:n]); err != nil {
			continue
		}
		if err := currentTrack.WriteRTP(pkt); err != nil {
			log.Printf("audio WriteRTP error: %v", err)
			return
		}
	}
}

func offerSupportsOpusAudio(desc *webrtc.SessionDescription) bool {
	if desc == nil || desc.SDP == "" {
		return false
	}
	sdp := strings.ToLower(desc.SDP)
	return strings.Contains(sdp, "m=audio") && strings.Contains(sdp, "opus/48000")
}

func mustJSON(v interface{}) json.RawMessage {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return b
}

