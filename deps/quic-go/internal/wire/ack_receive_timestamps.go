package wire

import (
	"github.com/quic-go/quic-go/internal/protocol"
	"github.com/quic-go/quic-go/quicvarint"
	"slices"
)

// ReceiveTimestamp implements the legacy mvfst ACK_RECEIVE_TIMESTAMPS (0xb0),
// not draft-ietf-quic-receive-ts. See the parent application's SBD report.
type ReceiveTimestamp struct {
	Packet protocol.PacketNumber
	Micros uint64
}

func (f *AckFrame) PrepareReceiveTimestamps() {
	slices.SortFunc(f.ReceiveTimestamps, func(a, b ReceiveTimestamp) int {
		if a.Packet > b.Packet {
			return -1
		}
		if a.Packet < b.Packet {
			return 1
		}
		return 0
	})
	// Legacy timestamp deltas are unsigned. Omit unrepresentable reordered
	// arrivals rather than falsifying delay; the sender detects incomplete data.
	out := f.ReceiveTimestamps[:0]
	for _, t := range f.ReceiveTimestamps {
		if !f.AcksPacket(t.Packet) {
			continue
		}
		if len(out) > 0 && (t.Packet == out[len(out)-1].Packet || t.Micros > out[len(out)-1].Micros) {
			continue
		}
		out = append(out, t)
	}
	f.ReceiveTimestamps = out
}

func (f *AckFrame) appendReceiveTimestamps(b []byte, version protocol.Version) ([]byte, error) {
	normal := *f
	normal.ReceiveTimestamps = nil
	normal.ECT0, normal.ECT1, normal.ECNCE = 0, 0, 0
	b = quicvarint.Append(b, 0xb0)
	start := len(b)
	var err error
	b, err = normal.Append(b, version)
	if err != nil {
		return nil, err
	}
	// Remove the ordinary one-byte ACK type after the extended frame type.
	copy(b[start:], b[start+1:])
	b = b[:len(b)-1]
	times := f.ReceiveTimestamps
	b = quicvarint.Append(b, uint64(times[0].Packet))
	b = quicvarint.Append(b, times[0].Micros)
	// A timestamp range represents contiguous packet numbers; the next range
	// skips at least one PN, using the same gap-minus-two rule as ACK ranges.
	ranges := 1
	for i := 1; i < len(times); i++ {
		if times[i-1].Packet-times[i].Packet != 1 {
			ranges++
		}
	}
	b = quicvarint.Append(b, uint64(ranges))
	previousTime := times[0].Micros * 2
	for start := 0; start < len(times); {
		end := start + 1
		for end < len(times) && times[end-1].Packet-times[end].Packet == 1 {
			end++
		}
		gap := uint64(0)
		if start > 0 {
			gap = uint64(times[start-1].Packet - times[start].Packet - 2)
		}
		b = quicvarint.Append(b, gap)
		b = quicvarint.Append(b, uint64(end-start))
		for _, t := range times[start:end] {
			b = quicvarint.Append(b, previousTime-t.Micros)
			previousTime = t.Micros
		}
		start = end
	}
	// 0xb0 has no ECN counters. Follow it with a standard ACK_ECN for the same
	// ranges so mvfst consumes timestamped outcomes first, then ECN feedback.
	if f.ECT0 > 0 || f.ECT1 > 0 || f.ECNCE > 0 {
		normal.ECT0, normal.ECT1, normal.ECNCE = f.ECT0, f.ECT1, f.ECNCE
		return normal.Append(b, version)
	}
	return b, nil
}

func (f *AckFrame) receiveTimestampsLength(version protocol.Version) protocol.ByteCount {
	normal := *f
	normal.ReceiveTimestamps = nil
	normal.ECT0, normal.ECT1, normal.ECNCE = 0, 0, 0
	length := int(normal.Length(version)) + 1 // two-byte extended type
	times := f.ReceiveTimestamps
	length += quicvarint.Len(uint64(times[0].Packet)) + quicvarint.Len(times[0].Micros)
	ranges := 1
	previous := times[0].Micros * 2
	for start := 0; start < len(times); {
		end := start + 1
		for end < len(times) && times[end-1].Packet-times[end].Packet == 1 {
			end++
		}
		gap := uint64(0)
		if start > 0 {
			gap = uint64(times[start-1].Packet - times[start].Packet - 2)
			ranges++
		}
		length += quicvarint.Len(gap) + quicvarint.Len(uint64(end-start))
		for _, t := range times[start:end] {
			length += quicvarint.Len(previous - t.Micros)
			previous = t.Micros
		}
		start = end
	}
	length += quicvarint.Len(uint64(ranges))
	if f.ECT0 > 0 || f.ECT1 > 0 || f.ECNCE > 0 {
		normal.ECT0, normal.ECT1, normal.ECNCE = f.ECT0, f.ECT1, f.ECNCE
		length += int(normal.Length(version))
	}
	return protocol.ByteCount(length)
}
