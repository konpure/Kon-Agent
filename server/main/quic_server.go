package main

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/binary"
	"encoding/pem"
	"flag"
	"fmt"
	"io"
	"log"
	"math/big"
	"sync"
	"time"

	"github.com/konpure/Kon-Agent/internal/transport/codec"
	"github.com/konpure/Kon-Agent/pkg/protocol"
	"github.com/quic-go/quic-go"
	"google.golang.org/protobuf/proto"
)

// vmAddr is the VictoriaMetrics base URL; empty disables storage (print-only
// mode). The accumulator converts agent-side delta series to cumulative.
var (
	vmAddr      = flag.String("vm", "http://localhost:8428", "VictoriaMetrics address (empty to disable)")
	accumulator = newDeltaAccumulator()
)

func main() {
	flag.Parse()

	// 生成自签名证书
	tlsCert, err := generateSelfSignedCert()
	if err != nil {
		log.Fatal("Failed to generate certificate:", err)
	}

	// TLS配置
	tlsConfig := &tls.Config{
		Certificates: []tls.Certificate{tlsCert},
		NextProtos:   []string{protocol.ALPN()},
		Rand:         rand.Reader,
		MinVersion:   tls.VersionTLS13,
		MaxVersion:   tls.VersionTLS13,
	}

	// QUIC监听配置
	quicConfig := &quic.Config{
		MaxIncomingStreams:    1000,
		MaxIncomingUniStreams: 1000,
		KeepAlivePeriod:       10 * time.Second,
	}

	// 监听QUIC连接
	listener, err := quic.ListenAddr(":7843", tlsConfig, quicConfig)
	if err != nil {
		log.Fatal("Failed to listen:", err)
	}
	defer listener.Close()

	fmt.Println("QUIC server listening on :7843")
	if *vmAddr != "" {
		fmt.Println("VictoriaMetrics storage:", *vmAddr)
	} else {
		fmt.Println("VictoriaMetrics storage disabled (print-only mode)")
	}

	for {
		// 接受新连接
		conn, err := listener.Accept(context.Background())
		if err != nil {
			log.Printf("Failed to accept connection: %v", err)
			continue
		}

		fmt.Println("New connection established")

		// 处理连接
		go handleConnection(conn)
	}
}

// 生成自签名证书
func generateSelfSignedCert() (tls.Certificate, error) {
	// 生成私钥
	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return tls.Certificate{}, err
	}

	// 创建证书模板
	template := x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject: pkix.Name{
			CommonName:   "localhost",
			Organization: []string{"Kon-Agent"},
		},
		NotBefore: time.Now(),
		NotAfter:  time.Now().Add(365 * 24 * time.Hour),
		KeyUsage:  x509.KeyUsageKeyEncipherment | x509.KeyUsageDigitalSignature,
		ExtKeyUsage: []x509.ExtKeyUsage{
			x509.ExtKeyUsageServerAuth,
		},
		BasicConstraintsValid: true,
		DNSNames:              []string{"localhost"},
	}

	// 创建自签名证书
	derBytes, err := x509.CreateCertificate(rand.Reader, &template, &template, &priv.PublicKey, priv)
	if err != nil {
		return tls.Certificate{}, err
	}

	// 编码证书和私钥
	certPEM := pem.EncodeToMemory(&pem.Block{
		Type:  "CERTIFICATE",
		Bytes: derBytes,
	})

	privBytes, err := x509.MarshalPKCS8PrivateKey(priv)
	if err != nil {
		return tls.Certificate{}, err
	}

	privPEM := pem.EncodeToMemory(&pem.Block{
		Type:  "PRIVATE KEY",
		Bytes: privBytes,
	})

	// 加载证书
	return tls.X509KeyPair(certPEM, privPEM)
}

// connState holds the per-connection stream decoder. The Gorilla codec keeps
// per-series state scoped to the connection; both sides start empty on
// (re)connect, so no out-of-band resync protocol is needed.
type connState struct {
	decoder *codec.StreamDecoder
	mu      sync.Mutex
}

// serverSupportedVersions lists the protocol versions this server accepts.
var serverSupportedVersions = []string{protocol.ProtocolVersion}

func handleConnection(conn interface{}) {
	// 在quic-go v0.54.0中，listener.Accept() 返回 *quic.Conn 类型
	quicConn, ok := conn.(*quic.Conn)
	if !ok {
		log.Printf("Invalid connection type: %T", conn)
		return
	}
	defer quicConn.CloseWithError(0, "")

	// 第一条流是双向握手流：校验协议版本，不兼容则拒绝连接
	if err := handleHandshake(quicConn); err != nil {
		log.Printf("Handshake failed: %v", err)
		return
	}

	state := &connState{decoder: codec.NewStreamDecoder()}

	for {
		// 接受新流 - 对于接收单向流，应该使用 AcceptUniStream
		stream, err := quicConn.AcceptUniStream(context.Background())
		if err != nil {
			log.Printf("Failed to accept unidirectional stream: %v", err)
			return
		}

		fmt.Printf("New unidirectional stream accepted: ID=%d\n", stream.StreamID())

		// 处理单向流
		go handleUniStream(quicConn, stream, state)
	}
}

// handleHandshake reads the client's HandshakeRequest on a bidirectional
// stream, validates the protocol version, and writes back a response.
func handleHandshake(conn *quic.Conn) error {
	stream, err := conn.AcceptStream(context.Background())
	if err != nil {
		return fmt.Errorf("failed to accept handshake stream: %w", err)
	}
	defer func() { _ = stream.Close() }()

	_ = stream.SetReadDeadline(time.Now().Add(10 * time.Second))

	var lengthBuf [4]byte
	if _, err := io.ReadFull(stream, lengthBuf[:]); err != nil {
		return fmt.Errorf("failed to read handshake length: %w", err)
	}
	length := binary.BigEndian.Uint32(lengthBuf[:])
	if length > 4096 {
		return fmt.Errorf("handshake too large: %d", length)
	}
	data := make([]byte, length)
	if _, err := io.ReadFull(stream, data); err != nil {
		return fmt.Errorf("failed to read handshake: %w", err)
	}

	var req protocol.HandshakeRequest
	if err := proto.Unmarshal(data, &req); err != nil {
		return fmt.Errorf("failed to unmarshal handshake: %w", err)
	}

	fmt.Printf("Handshake: agent=%s, protocol=%s\n", req.GetAgentId(), req.GetProtocolVersion())

	// 版本校验：当前只接受完全匹配
	accepted := false
	for _, v := range serverSupportedVersions {
		if req.GetProtocolVersion() == v {
			accepted = true
			break
		}
	}

	resp := &protocol.HandshakeResponse{
		Accepted:          accepted,
		ServerVersion:     protocol.ProtocolVersion,
		SupportedVersions: serverSupportedVersions,
	}
	if !accepted {
		resp.Message = fmt.Sprintf("unsupported protocol version %q", req.GetProtocolVersion())
	}

	respData, err := proto.Marshal(resp)
	if err != nil {
		return fmt.Errorf("failed to marshal handshake response: %w", err)
	}
	frame := make([]byte, 4+len(respData))
	binary.BigEndian.PutUint32(frame[:4], uint32(len(respData)))
	copy(frame[4:], respData)

	if _, err := stream.Write(frame); err != nil {
		return fmt.Errorf("failed to send handshake response: %w", err)
	}

	if !accepted {
		return fmt.Errorf("rejected client protocol %q", req.GetProtocolVersion())
	}
	return nil
}

func handleUniStream(conn *quic.Conn, stream *quic.ReceiveStream, state *connState) {
	// 在quic-go v0.54.0中，ReceiveStream可能没有Close方法
	// 使用stream.CancelRead()来取消读取并释放资源
	defer stream.CancelRead(0)

	// 直接使用stream指针的方法来读取数据
	reader := stream

	for {
		// 读取4字节的长度前缀
		var lengthBuf [4]byte
		_, err := io.ReadFull(reader, lengthBuf[:])
		if err != nil {
			if err == io.EOF {
				fmt.Printf("Stream %d closed normally\n", stream.StreamID())
				return
			}
			log.Printf("Failed to read length prefix from stream %d: %v", stream.StreamID(), err)
			return
		}

		// 解析长度（覆盖 flags + payload）
		length := binary.BigEndian.Uint32(lengthBuf[:])
		if length > 10*1024*1024 || length < 1 { // 限制最大10MB
			log.Printf("Data too large from stream %d: %d bytes", stream.StreamID(), length)
			return
		}

		// 读取帧体：首字节为编码标志位
		frame := make([]byte, length)
		_, err = io.ReadFull(reader, frame)
		if err != nil {
			log.Printf("Failed to read data from stream %d: %v", stream.StreamID(), err)
			return
		}
		flags := frame[0]
		payload := frame[1:]

		// 按 flags 解码；Gorilla 帧需要连接级解码器状态
		state.mu.Lock()
		req, err := state.decoder.Decode(flags, payload)
		state.mu.Unlock()
		if err != nil {
			// 解码失败意味着双端流状态失同步。关闭连接强制客户端重连：
			// 重连后编码器重置、所有序列重发关键帧，实现无协议开销的自愈。
			log.Printf("Failed to decode frame (flags=%d) from stream %d: %v — closing connection to force codec resync", flags, stream.StreamID(), err)
			_ = conn.CloseWithError(0, "codec resync")
			return
		}
		printExportRequest(stream.StreamID(), req)

		// 写入 VictoriaMetrics；失败只记日志，不阻塞接收主路
		if *vmAddr != "" {
			lines := convertToVMLines(req, accumulator)
			writeCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			if err := postToVM(writeCtx, *vmAddr, lines); err != nil {
				log.Printf("Failed to write to VictoriaMetrics: %v", err)
			}
			cancel()
		}
	}
}

// printExportRequest prints the OTLP-style three-layer structure for debugging.
func printExportRequest(streamID quic.StreamID, req *protocol.ExportMetricsRequest) {
	fmt.Printf("Received ExportMetricsRequest from stream %d:\n", streamID)
	if res := req.GetResource(); res != nil {
		fmt.Printf("Agent ID: %s\n", res.GetAgentId())
		fmt.Printf("Resource attributes: %v\n", res.GetAttributes())
	}
	fmt.Printf("Export time: %d\n", req.GetExportTimeUnixNano())
	for _, sm := range req.GetScopeMetrics() {
		fmt.Printf("  Scope: %s, metrics: %d\n", sm.GetScope().GetName(), len(sm.GetMetrics()))
		for _, m := range sm.GetMetrics() {
			fmt.Printf("    %s (unit: %q): %s, points: %d\n",
				m.GetName(), m.GetUnit(), dataKind(m), dataPointCount(m))
		}
	}
	fmt.Println("---")
}

func dataKind(m *protocol.Metric) string {
	switch m.GetData().(type) {
	case *protocol.Metric_Gauge:
		return "Gauge"
	case *protocol.Metric_Sum:
		return fmt.Sprintf("Sum(temporality=%s, monotonic=%t)",
			m.GetSum().GetTemporality(), m.GetSum().GetIsMonotonic())
	case *protocol.Metric_Histogram:
		return "Histogram"
	default:
		return "Unknown"
	}
}

func dataPointCount(m *protocol.Metric) int {
	switch d := m.GetData().(type) {
	case *protocol.Metric_Gauge:
		return len(d.Gauge.GetDataPoints())
	case *protocol.Metric_Sum:
		return len(d.Sum.GetDataPoints())
	case *protocol.Metric_Histogram:
		return len(d.Histogram.GetDataPoints())
	default:
		return 0
	}
}
