package quic

import (
	"context"
	"crypto/tls"
	"fmt"
	"github.com/konpure/Kon-Agent/pkg/protocol"
	"github.com/quic-go/quic-go"
	"google.golang.org/protobuf/proto"
	"log/slog"
	"strings"
	"sync"
	"time"
)

type Client struct {
	serverAddr string
	tlsConfig  *tls.Config
	quicConfig *quic.Config
	conn       *quic.Conn
	mutex      sync.Mutex

	// Connection Pool
	streamPool      chan *quic.SendStream
	maxPoolSize     int
	streamTimeout   time.Duration
	poolInitialized bool
}

func NewClient(serverAddr string) *Client {
	maxPoolSize := 10
	streamTimeout := 10 * time.Second

	return &Client{
		serverAddr: serverAddr,
		tlsConfig: &tls.Config{
			InsecureSkipVerify: true, // For development only
			NextProtos:         []string{"kon-agent"},
		},
		quicConfig: &quic.Config{
			KeepAlivePeriod: 10 * time.Second,
		},
		maxPoolSize:   maxPoolSize,
		streamTimeout: streamTimeout,
		streamPool:    make(chan *quic.SendStream, maxPoolSize),
	}
}

func (c *Client) initStreamPool() {
	c.mutex.Lock()
	defer c.mutex.Unlock()

	if c.poolInitialized || c.conn == nil {
		return
	}

	c.poolInitialized = true

	go func() {
		ticker := time.NewTicker(c.streamTimeout / 2)
		defer ticker.Stop()

		for {
			select {
			case <-ticker.C:
				// TODO: Check if stream is still active
				c.mutex.Lock()
				conn := c.conn
				c.mutex.Unlock()

				if conn == nil {
					break
				}

				tempPool := make([]*quic.SendStream, 0)
				poolSize := len(c.streamPool)

				// Check if stream is still active
				for i := 0; i < poolSize; i++ {
					stream := <-c.streamPool
					if stream != nil && stream.Context().Err() == nil {
						tempPool = append(tempPool, stream)
					} else if stream != nil {
						stream.Close()
					}
				}

				for _, stream := range tempPool {
					select {
					case c.streamPool <- stream:
						// stream has been returned to pool
					default:
						// stream is not returned to pool, close it
						stream.Close()
					}
				}
			}
		}
	}()
}

func (c *Client) getStream(ctx context.Context) (*quic.SendStream, error) {
	select {
	case stream := <-c.streamPool:
		if stream.Context().Err() == nil {
			return stream, nil
		}
		return c.createNewStream(ctx)
	default:
		return c.createNewStream(ctx)
	}
}

func (c *Client) createNewStream(ctx context.Context) (*quic.SendStream, error) {
	c.mutex.Lock()
	conn := c.conn
	c.mutex.Unlock()

	if conn == nil {
		return nil, fmt.Errorf("Not connected to QUIC server")
	}
	return conn.OpenUniStreamSync(ctx)
}

func (c *Client) Connect(ctx context.Context) error {
	c.mutex.Lock()
	defer c.mutex.Unlock()

	// Remove "quic://" prefix if present
	serverAddr := c.serverAddr
	if strings.HasPrefix(serverAddr, "quic://") {
		serverAddr = strings.TrimPrefix(serverAddr, "quic://")
	}

	slog.Info("Connecting to QUIC server", "addr", serverAddr)

	conn, err := quic.DialAddr(ctx, serverAddr, c.tlsConfig, c.quicConfig)
	if err != nil {
		return fmt.Errorf("Failed to dial QUIC server: %w", err)
	}

	c.conn = conn
	slog.Info("Connected to QUIC server successfully", "addr", serverAddr)

	// Initialize stream pool
	go c.initStreamPool()

	return nil
}

func (c *Client) SendMetric(ctx context.Context, metric *protocol.Metric) error {
	c.mutex.Lock()
	conn := c.conn
	c.mutex.Unlock()

	if conn == nil {
		return fmt.Errorf("Not connected to QUIC server")
	}

	stream, err := c.getStream(ctx)
	if err != nil {
		if isConnectionClosed(err) {
			c.mutex.Lock()
			c.conn = nil
			c.mutex.Unlock()
		}
		return fmt.Errorf("Failed to open stream: %w", err)
	}

	defer func() {
		if stream.Context().Err() == nil {
			select {
			case c.streamPool <- stream:
				// stream has been returned to pool
			default:
				// stream is not returned to pool, close it
				stream.Close()
			}
		} else {
			stream.Close()
		}
	}()

	data, err := proto.Marshal(metric)
	if err != nil {
		return fmt.Errorf("Failed to marshal metric: %w", err)
	}

	done := make(chan error, 1)
	go func() {
		length := uint32(len(data))
		lengthBuf := make([]byte, 4)
		lengthBuf[0] = byte(length >> 24)
		lengthBuf[1] = byte(length >> 16)
		lengthBuf[2] = byte(length >> 8)
		lengthBuf[3] = byte(length)

		if _, err := stream.Write(lengthBuf); err != nil {
			done <- fmt.Errorf("Failed to write length: %w", err)
			return
		}

		if _, err := stream.Write(data); err != nil {
			done <- fmt.Errorf("Failed to write data: %w", err)
			return
		}

		done <- nil
	}()

	select {
	case err := <-done:
		if err != nil {
			if isConnectionClosed(err) {
				c.mutex.Lock()
				c.conn = nil
				c.mutex.Unlock()
			}
			return fmt.Errorf("Failed to send metric: %w", err)
		}
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (c *Client) SendBatchMetrics(ctx context.Context, req *protocol.BatchMetricsRequest) error {
	c.mutex.Lock()
	conn := c.conn
	c.mutex.Unlock()

	if conn == nil {
		return fmt.Errorf("Not connected to QUIC server")
	}

	stream, err := c.getStream(ctx)
	if err != nil {
		if isConnectionClosed(err) {
			c.mutex.Lock()
			c.conn = nil
			c.mutex.Unlock()
		}
		return fmt.Errorf("Failed to open stream: %w", err)
	}

	defer func() {
		if stream.Context().Err() == nil {
			select {
			case c.streamPool <- stream:
				// stream has been returned to pool
			default:
				// stream is not returned to pool, close it
				stream.Close()
			}
		} else {
			stream.Context()
		}
	}()

	data, err := proto.Marshal(req)
	if err != nil {
		return fmt.Errorf("Failed to marshal batch metrics: %w", err)
	}

	length := uint32(len(data))
	lengthBuf := make([]byte, 4)
	lengthBuf[0] = byte(length >> 24)
	lengthBuf[1] = byte(length >> 16)
	lengthBuf[2] = byte(length >> 8)
	lengthBuf[3] = byte(length)

	if _, err := stream.Write(lengthBuf); err != nil {
		return fmt.Errorf("Failed to write length: %w", err)
	}

	if _, err := stream.Write(data); err != nil {
		return fmt.Errorf("Failed to write data: %w", err)
	}
	return nil
}

func (c *Client) Close() error {
	c.mutex.Lock()
	defer c.mutex.Unlock()

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
