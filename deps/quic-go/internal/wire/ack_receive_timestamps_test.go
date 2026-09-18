package wire

import (
	"github.com/quic-go/quic-go/internal/protocol"
	"github.com/quic-go/quic-go/quicvarint"
	"testing"
)

func TestSbdTimestampWire(t *testing.T) {
	f := &AckFrame{AckRanges: []AckRange{{Smallest: 1, Largest: 5}}, ECT1: 5,
		ReceiveTimestamps: []ReceiveTimestamp{{Packet: 5, Micros: 2000}, {Packet: 4, Micros: 1900}, {Packet: 1, Micros: 1200}}}
	b, err := f.Append(nil, protocol.Version1)
	if err != nil {
		t.Fatal(err)
	}
	if len(b) != int(f.Length(protocol.Version1)) {
		t.Fatal("length mismatch")
	}
	// Independent wire expectations: type, ACK header, latest PN/time, two
	// timestamp ranges (5,4) and (1), gap-minus-two, then ACK_ECN counters.
	expected := []uint64{0xb0, 5, 0, 0, 4, 5, 2000, 2, 0, 2, 2000, 100, 1, 1, 700, 3, 5, 0, 0, 4, 0, 5, 0}
	for _, want := range expected {
		got, n, err := quicvarint.Parse(b)
		if err != nil || got != want {
			t.Fatalf("got %d, want %d: %v", got, want, err)
		}
		b = b[n:]
	}
	if len(b) != 0 {
		t.Fatal("unexpected bytes")
	}
	f.Truncate(30, protocol.Version1)
	if f.Length(protocol.Version1) > 30 {
		t.Fatal("oversized ACK")
	}
	f.Reset()
	if len(f.ReceiveTimestamps) != 0 {
		t.Fatal("pooled frame retained timestamps")
	}
}
func TestSbdReorderedTimestampNotFabricated(t *testing.T) {
	f := &AckFrame{AckRanges: []AckRange{{Smallest: 1, Largest: 3}}, ReceiveTimestamps: []ReceiveTimestamp{
		{Packet: 1, Micros: 100}, {Packet: 2, Micros: 300}, {Packet: 3, Micros: 200},
	}}
	f.PrepareReceiveTimestamps()
	if len(f.ReceiveTimestamps) != 2 || f.ReceiveTimestamps[1].Packet != 1 {
		t.Fatal(f.ReceiveTimestamps)
	}
}
func TestSbdNegotiation(t *testing.T) {
	p := &TransportParameters{ActiveConnectionIDLimit: 2, MaxUDPPayloadSize: 1200}
	data := p.Marshal(protocol.PerspectiveClient)
	var parsed TransportParameters
	if err := parsed.Unmarshal(data, protocol.PerspectiveClient); err != nil {
		t.Fatal(err)
	}
	if parsed.ReceiveTimestampsEnabled != 1 || parsed.MaxReceiveTimestamps != 0 {
		t.Fatal("client must advertise send-only support")
	}
}

func BenchmarkSbdFeedback256(b *testing.B) {
	f := &AckFrame{AckRanges: []AckRange{{Smallest: 1, Largest: 256}}, ECT1: 256}
	for i := 0; i < 256; i++ {
		f.ReceiveTimestamps = append(f.ReceiveTimestamps, ReceiveTimestamp{Packet: protocol.PacketNumber(256 - i), Micros: uint64(100000 - i*100)})
	}
	buf := make([]byte, 0, 2000)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		f.PrepareReceiveTimestamps()
		_ = f.Length(protocol.Version1)
		var err error
		buf, err = f.Append(buf[:0], protocol.Version1)
		if err != nil {
			b.Fatal(err)
		}
	}
}
