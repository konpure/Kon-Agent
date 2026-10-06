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
	"fmt"
	"io"
	"log"
	"math/big"
	"time"

	"github.com/konpure/Kon-Agent/pkg/protocol"
	"github.com/quic-go/quic-go"
	"google.golang.org/protobuf/proto"
)

func main() {
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

func handleConnection(conn interface{}) {
	// 在quic-go v0.54.0中，listener.Accept() 返回 *quic.Conn 类型
	quicConn, ok := conn.(*quic.Conn)
	if !ok {
		log.Printf("Invalid connection type: %T", conn)
		return
	}
	defer quicConn.CloseWithError(0, "")

	for {
		// 接受新流 - 对于接收单向流，应该使用 AcceptUniStream
		stream, err := quicConn.AcceptUniStream(context.Background())
		if err != nil {
			log.Printf("Failed to accept unidirectional stream: %v", err)
			return
		}

		fmt.Printf("New unidirectional stream accepted: ID=%d\n", stream.StreamID())

		// 处理单向流
		go handleUniStream(stream)
	}
}

func handleUniStream(stream *quic.ReceiveStream) {
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

		// 解析长度
		length := binary.BigEndian.Uint32(lengthBuf[:])
		if length > 10*1024*1024 { // 限制最大10MB
			log.Printf("Data too large from stream %d: %d bytes", stream.StreamID(), length)
			return
		}

		// 读取实际数据
		data := make([]byte, length)
		_, err = io.ReadFull(reader, data)
		if err != nil {
			log.Printf("Failed to read data from stream %d: %v", stream.StreamID(), err)
			return
		}

		// 解析Protobuf数据（协议 v2）
		var req protocol.ExportMetricsRequest
		if err := proto.Unmarshal(data, &req); err != nil {
			log.Printf("Failed to unmarshal ExportMetricsRequest from stream %d: %v", stream.StreamID(), err)
			continue
		}
		printExportRequest(stream.StreamID(), &req)
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
