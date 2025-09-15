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
		NextProtos:   []string{"kon-agent"},
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

		// 解析Protobuf数据
		var batchReq protocol.BatchMetricsRequest
		if err := proto.Unmarshal(data, &batchReq); err != nil {
			// 如果不是BatchMetricsRequest，尝试解析为单个Metric
			var metric protocol.Metric
			if err := proto.Unmarshal(data, &metric); err != nil {
				log.Printf("Failed to unmarshal data from stream %d: %v", stream.StreamID(), err)
				// 输出原始数据供调试
				fmt.Printf("Received from stream %d:\n", stream.StreamID())
				fmt.Printf("Hex: %x\n", data)
				fmt.Printf("Raw (binary data, may contain garbled text): %s\n", string(data))
				fmt.Println("---")
				continue
			}
			// 成功解析为单个Metric
			fmt.Printf("Received Metric from stream %d:\n", stream.StreamID())
			fmt.Printf("Name: %s\n", metric.Name)
			fmt.Printf("Value: %.2f\n", metric.Value)
			fmt.Printf("Timestamp: %d\n", metric.Timestamp)
			fmt.Printf("Type: %s\n", metric.Type.String())
			if len(metric.Labels) > 0 {
				fmt.Printf("Labels: %v\n", metric.Labels)
			}
			fmt.Println("---")
		} else {
			// 成功解析为BatchMetricsRequest
			fmt.Printf("Received BatchMetricsRequest from stream %d:\n", stream.StreamID())
			fmt.Printf("Agent ID: %s\n", batchReq.AgentId)
			fmt.Printf("Timestamp: %d\n", batchReq.Timestamp)
			fmt.Printf("Metrics count: %d\n", len(batchReq.Metrics))
			for i, metric := range batchReq.Metrics {
				fmt.Printf("  Metric %d: %s=%.2f (type: %s)\n", i+1, metric.Name, metric.Value, metric.Type.String())
			}
			fmt.Println("---")
		}
	}
}
