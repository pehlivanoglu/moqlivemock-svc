package sub

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/Eyevinn/moqlivemock/internal"
	"github.com/Eyevinn/moqtransport"
	"github.com/stretchr/testify/require"
)

func playbackCatalog() *internal.Catalog {
	return &internal.Catalog{Tracks: []internal.Track{
		{Name: "video/s0", Role: "video", Packaging: "loc", Codec: "av01.0.04M.08",
			SpatialID: internal.Ptr(0), Width: internal.Ptr(640), Height: internal.Ptr(360), Bitrate: internal.Ptr(100_000)},
		{Name: "video/s1", Role: "video", Packaging: "loc", Codec: "av01.0.04M.08",
			Dependencies: []string{"video/s0"}, SpatialID: internal.Ptr(1),
			Width: internal.Ptr(1280), Height: internal.Ptr(720), Bitrate: internal.Ptr(200_000)},
		{Name: "video/s2", Role: "video", Packaging: "loc", Codec: "av01.0.04M.08",
			Dependencies: []string{"video/s1"}, SpatialID: internal.Ptr(2),
			Width: internal.Ptr(1920), Height: internal.Ptr(1080), Bitrate: internal.Ptr(300_000)},
	}}
}

func newTestPlayback(t *testing.T, minimum time.Duration) *nativePlayback {
	t.Helper()
	p, err := newNativePlayback(playbackCatalog(),
		[]string{"video/s0", "video/s1", "video/s2"}, "video/s2", true,
		minimum, 300*time.Millisecond, "")
	require.NoError(t, err)
	return p
}

func playbackHeaders(spatialID int, timestamp time.Time, independent bool) moqtransport.KVPList {
	flags := uint64(0xc0)
	if independent {
		flags |= 0x20
	}
	return moqtransport.KVPList{
		{Type: locPropFrameMark, ValueVarInt: flags<<8 | uint64(spatialID)},
		{Type: locPropTimestamp, ValueVarInt: uint64(timestamp.UnixMicro())},
	}
}

func addPlaybackUnit(p *nativePlayback, object uint64, timestamp time.Time,
	independent bool, layers []int, arrival time.Time,
) {
	for _, layer := range layers {
		p.addObject(p.tracks[layer].name, 7, object,
			playbackHeaders(layer, timestamp, independent), layer+1, arrival)
	}
}

func TestNativePlaybackStartsOnBestBufferedIndependentChain(t *testing.T) {
	p := newTestPlayback(t, 100*time.Millisecond)
	start := time.Unix(1_700_000_000, 0)

	addPlaybackUnit(p, 0, start, true, []int{2, 0, 1}, start.Add(20*time.Millisecond))
	addPlaybackUnit(p, 1, start.Add(100*time.Millisecond), false, []int{0, 1, 2}, start.Add(120*time.Millisecond))
	p.advance(start.Add(300 * time.Millisecond))

	metrics := p.snapshot(start.Add(300 * time.Millisecond))
	require.Equal(t, "playing", metrics.State)
	require.Equal(t, "video/s2", *metrics.ActiveTrack)
	require.Equal(t, 2, *metrics.Quality.SpatialID)
	require.Equal(t, 600_000, *metrics.CatalogBitrateBPS)
}

func TestNativePlaybackDownswitchPersistsUntilIndependentUnit(t *testing.T) {
	p := newTestPlayback(t, 0)
	start := time.Unix(1_700_000_000, 0)
	addPlaybackUnit(p, 0, start, true, []int{0, 1, 2}, start)
	p.advance(start.Add(300 * time.Millisecond))

	addPlaybackUnit(p, 1, start.Add(33*time.Millisecond), false, []int{0, 1}, start)
	p.advance(start.Add(333 * time.Millisecond))
	require.Equal(t, 1, p.active)
	require.False(t, p.valid[2])

	addPlaybackUnit(p, 2, start.Add(66*time.Millisecond), false, []int{0, 1, 2}, start)
	p.advance(start.Add(366 * time.Millisecond))
	require.Equal(t, 1, p.active, "complete delta must not upswitch")

	addPlaybackUnit(p, 3, start.Add(99*time.Millisecond), true, []int{0, 1, 2}, start)
	p.advance(start.Add(399 * time.Millisecond))
	require.Equal(t, 2, p.active)
	require.Equal(t, "stable", p.switchState)
}

func TestNativePlaybackMissingBaseStallsAndRecoversAtIndependentUnit(t *testing.T) {
	p := newTestPlayback(t, 0)
	start := time.Unix(1_700_000_000, 0)
	addPlaybackUnit(p, 0, start, true, []int{0, 1, 2}, start)
	p.advance(start.Add(300 * time.Millisecond))

	addPlaybackUnit(p, 1, start.Add(33*time.Millisecond), false, []int{1, 2}, start)
	p.advance(start.Add(333 * time.Millisecond))
	require.Equal(t, -1, p.active)
	require.Equal(t, "stalled", p.state)
	require.Equal(t, 1, p.stallCount)
	frozenPlayhead := p.playheadTimestamp(start.Add(333 * time.Millisecond))
	require.Equal(t, frozenPlayhead, p.playheadTimestamp(start.Add(500*time.Millisecond)))

	addPlaybackUnit(p, 2, start.Add(66*time.Millisecond), true, []int{0, 1}, start)
	p.advance(start.Add(366 * time.Millisecond))
	require.Equal(t, 1, p.active)
	require.Equal(t, "playing", p.state)
	require.Equal(t, "waiting_independent", p.switchState)
	require.Greater(t, p.stallDuration, time.Duration(0))
}

func TestNativePlaybackWritesAtomicMetrics(t *testing.T) {
	path := filepath.Join(t.TempDir(), "metrics.json")
	p, err := newNativePlayback(playbackCatalog(),
		[]string{"video/s0", "video/s1", "video/s2"}, "video/s2", false,
		0, 0, path)
	require.NoError(t, err)
	now := time.Unix(1_700_000_000, 0)
	p.addObject("video/s0", 1, 1, nil, 125, now)
	p.writeMetrics(now)

	data, err := os.ReadFile(path)
	require.NoError(t, err)
	var metrics playbackMetrics
	require.NoError(t, json.Unmarshal(data, &metrics))
	require.Equal(t, "receiving", metrics.State)
	require.Equal(t, float64(1000), metrics.ReceiveBitrateBPS)
	_, err = os.Stat(path + ".tmp")
	require.ErrorIs(t, err, os.ErrNotExist)
}

func TestNativePlaybackLatencyAndBufferMetricsFollowPlaybackState(t *testing.T) {
	p := newTestPlayback(t, 0)
	start := time.Unix(1_700_000_000, 0)
	addPlaybackUnit(p, 0, start, true, []int{0, 1, 2}, start)
	addPlaybackUnit(p, 1, start.Add(100*time.Millisecond), false, []int{0, 1, 2}, start)
	p.advance(start.Add(300 * time.Millisecond))

	first := p.snapshot(start.Add(310 * time.Millisecond))
	require.InDelta(t, 310, *first.E2ELatencyMS, 0.001)
	require.InDelta(t, 90, *first.BufferLevelMS, 0.001)

	addPlaybackUnit(p, 2, start.Add(200*time.Millisecond), false, []int{0, 1, 2}, start)
	second := p.snapshot(start.Add(350 * time.Millisecond))
	require.Greater(t, *second.E2ELatencyMS, *first.E2ELatencyMS)
	require.Greater(t, *second.BufferLevelMS, *first.BufferLevelMS)
}

func TestNativePlaybackPresentsLateFramesAtFixedRate(t *testing.T) {
	p := newTestPlayback(t, 0)
	start := time.Unix(1_700_000_000, 0)
	addPlaybackUnit(p, 0, start, true, []int{0, 1, 2}, start)
	addPlaybackUnit(p, 1, start.Add(40*time.Millisecond), false, []int{0, 1, 2}, start)
	addPlaybackUnit(p, 2, start.Add(80*time.Millisecond), false, []int{0, 1, 2}, start)
	p.advance(start.Add(300 * time.Millisecond))

	late := start.Add(time.Second)
	p.advance(late)
	require.Equal(t, uint64(start.Add(40*time.Millisecond).UnixMicro()), *p.lastPresented)
	require.Contains(t, p.pending, playbackUnitKey{group: 7, object: 2})

	p.advance(late.Add(39 * time.Millisecond))
	require.Equal(t, uint64(start.Add(40*time.Millisecond).UnixMicro()), *p.lastPresented)
	p.advance(late.Add(40 * time.Millisecond))
	require.Equal(t, uint64(start.Add(80*time.Millisecond).UnixMicro()), *p.lastPresented)
}

func TestNativePlaybackBoundsPendingUnits(t *testing.T) {
	p := newTestPlayback(t, 0)
	start := time.Unix(1_700_000_000, 0)
	for object := 0; object <= maxPendingUnits; object++ {
		addPlaybackUnit(p, uint64(object), start.Add(time.Duration(object)*time.Millisecond),
			false, []int{2}, start)
	}

	require.Len(t, p.pending, maxPendingUnits)
}
