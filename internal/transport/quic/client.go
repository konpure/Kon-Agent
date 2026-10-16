package quic

import (
	"context"
	"crypto/tls"
	"encoding/binary"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/konpure/Kon-Agent/pkg/protocol"
	"github.com/quic-go/quic-go"
	"google.golang.org/protobuf/proto"
)

// Client ships frames over a single persistent uni-directional stream.
//
// Why one stream instead of a pool: the Gorilla-style codec keeps per-series
// state scoped to a connection, so frames must arrive in send order. QUIC
// makes no ordering guarantee across streams, but a single stream is an
// ordered byte pipe — the decoder on the other end can safely apply frames
// to its mirror state.
type Client struct {
	serverAddr string
	tlsConfig  *tls.Config
	quicConfig *quic.Config

	mutex      sync.Mutex
	conn       *quic.Conn
	dataStream *quic.SendStream
}

func NewClient(serverAddr string) *Client {
	return &Client{
		serverAddr: serverAddr,
		tlsConfig: &tls.Config{
			InsecureSkipVerify: true, // For development only
			NextProtos:         []string{protocol.ALPN()},
		},
		quicConfig: &quic.Config{
			KeepAlivePeriod: 10 * time.Second,
		},
	}
}

func (c *Client) Connect(ctx context.Context) error {
	c.mutex.Lock()
	defer c.mutex.Unlock()

	serverAddr := strings.TrimPrefix(c.serverAddr, "quic://")

	slog.Info("Connecting to QUIC server", "addr", serverAddr)

	conn, err := quic.DialAddr(ctx, serverAddr, c.tlsConfig, c.quicConfig)
	if err != nil {
		return fmt.Errorf("failed to dial QUIC server: %w", err)
	}

	c.conn = conn
	// Fresh connection: no data stream yet, and the codec state on both
	// sides starts empty (the exporter resets its encoder on reconnect).
	c.dataStream = nil
	slog.Info("Connected to QUIC server successfully", "addr", serverAddr)
	return nil
}

// SendBatchMetrics marshals the request and ships it as a raw-proto frame.
func (c *Client) SendBatchMetrics(ctx context.Context, req *protocol.ExportMetricsRequest) error {
	data, err := proto.Marshal(req)
	if err != nil {
		return fmt.Errorf("failed to marshal batch metrics: %w", err)
	}
	return c.SendRaw(ctx, 0, data)
}

// SendRaw ships one frame over the persistent data stream.
// Wire format: [length:4][flags:1][payload]; length covers flags+payload.
//
// On write failure the stream may hold a partial frame, which corrupts the
// ordered byte pipe — the caller (exporter) is expected to close the
// connection so both sides reset their codec state.
func (c *Client) SendRaw(ctx context.Context, flags uint8, data []byte) error {
	stream, err := c.getDataStream(ctx)
	if err != nil {
		return err
	}

	frame := make([]byte, 5+len(data))
	binary.BigEndian.PutUint32(frame[:4], uint32(len(data)+1))
	frame[4] = flags
	copy(frame[5:], data)

	if _, err := stream.Write(frame); err != nil {
		c.dropDataStream()
		return fmt.Errorf("failed to write frame: %w", err)
	}
	return nil
}

// getDataStream returns the live data stream, opening one if needed.
func (c *Client) getDataStream(ctx context.Context) (*quic.SendStream, error) {
	c.mutex.Lock()
	conn := c.conn
	stream := c.dataStream
	c.mutex.Unlock()

	if conn == nil {
		return nil, fmt.Errorf("not connected to QUIC server")
	}
	if stream != nil && stream.Context().Err() == nil {
		return stream, nil
	}

	newStream, err := conn.OpenUniStreamSync(ctx)
	if err != nil {
		if isConnectionClosed(err) {
			c.mutex.Lock()
			c.conn = nil
			c.dataStream = nil
			c.mutex.Unlock()
		}
		return nil, fmt.Errorf("failed to open stream: %w", err)
	}

	c.mutex.Lock()
	c.dataStream = newStream
	c.mutex.Unlock()
	return newStream, nil
}

// dropDataStream discards the current stream after a write failure.
func (c *Client) dropDataStream() {
	c.mutex.Lock()
	defer c.mutex.Unlock()
	if c.dataStream != nil {
		_ = c.dataStream.Close()
		c.dataStream = nil
	}
}

func (c *Client) Close() error {
	c.mutex.Lock()
	defer c.mutex.Unlock()

	if c.dataStream != nil {
		_ = c.dataStream.Close()
		c.dataStream = nil
	}
	if c.conn != nil {
		err := c.conn.CloseWithError(0, "client closing")
		c.conn = nil
		return err
	}
	return nil
}

func (c *Client) IsConnected() bool {
	c.mutex.Lock()
	defer c.mutex.Unlock()
	return c.conn != nil && c.conn.Context().Err() == nil
}

func isConnectionClosed(err error) bool {
	if err == nil {
		return false
	}
	s := err.Error()
	return strings.Contains(s, "APPLICATION_ERROR") ||
		strings.Contains(s, "CONNECTION_CLOSED") ||
		strings.Contains(s, "CONNECTION_IDLE") ||
		strings.Contains(s, "TIMEOUT")
}
