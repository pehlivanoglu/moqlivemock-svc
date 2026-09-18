# Research fork: RFC 8382 receive timestamp feedback

Base: github.com/quic-go/quic-go v0.60.0, copied from the checksum-verified Go
module cache. Original license and source are retained. The parent application
selects this local fork through `go.mod`; normal native Docker builds include it.

Changes are restricted to send-only support for the pinned mvfst legacy
ACK_RECEIVE_TIMESTAMPS extension (0xb0 and transport parameters 0xff0a001–3):

- Client advertises support, requesting zero timestamps in the reverse direction.
- A peer request enables a bounded 256-entry packet-arrival timestamp ring.
- ACKs carry packet-number/timestamp ranges at the negotiated resolution.
- ACK_ECN follows a timestamped ACK where counters need reporting.
- Invalid negotiation values are rejected; frame-size limits are respected.
- Unrepresentable reordered arrivals are omitted, never assigned fake timestamps.
- Handshake ACKs and unnegotiated connections retain their original encoding.

This is not an implementation of the current QUIC receive-timestamps draft.
It does not change congestion control, media selection, or ECN codepoint selection.
The relay stops OWD measurement if timestamp coverage becomes incomplete.

Tests: `go test ./...` in this directory. `internal/wire/ack_receive_timestamps_test.go`
contains independent wire-vector, negotiation, truncation and reordering checks.
See the relay repository's `rfc-8382-sbd-implementation.md` for integration results.
