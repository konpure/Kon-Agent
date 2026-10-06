package protocol

// ProtocolVersion is the wire protocol version, following Prometheus
// Remote Write's semantic-versioning practice: additive changes bump the
// minor version, breaking changes bump the major version. Both peers
// advertise it via the QUIC ALPN token.
const ProtocolVersion = "2.0"

// ALPN returns the QUIC ALPN token carrying the protocol version.
func ALPN() string {
	return "kon-agent/" + ProtocolVersion
}
