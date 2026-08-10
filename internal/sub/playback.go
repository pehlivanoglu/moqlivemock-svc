package sub

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/Eyevinn/moqlivemock/internal"
	"github.com/Eyevinn/moqtransport"
)

const (
	metricsSchemaVersion = 1
	maxPendingUnits      = 64
)

type playbackTrack struct {
	name      string
	spatialID int
	width     *int
	height    *int
	bitrate   int
}

type playbackUnitKey struct {
	group  uint64
	object uint64
}

type playbackLayer struct {
	timestampUs uint64
	independent bool
	bytes       int
}

type playbackUnit struct {
	key         playbackUnitKey
	layers      map[int]playbackLayer
	invalidFrom int
}

type timedBytes struct {
	at    time.Time
	bytes int
}

type playbackQuality struct {
	SpatialID *int `json:"spatial_id"`
	Width     *int `json:"width"`
	Height    *int `json:"height"`
}

type playbackMetrics struct {
	SchemaVersion     int             `json:"schema_version"`
	SampledAtUnixMS   int64           `json:"sampled_at_unix_ms"`
	Client            string          `json:"client"`
	Simulated         bool            `json:"simulated"`
	State             string          `json:"state"`
	TargetTrack       *string         `json:"target_track"`
	ActiveTrack       *string         `json:"active_track"`
	SwitchState       string          `json:"switch_state"`
	Quality           playbackQuality `json:"quality"`
	E2ELatencyMS      *float64        `json:"e2e_latency_ms"`
	PlayerBitrateBPS  *float64        `json:"player_bitrate_bps"`
	ReceiveBitrateBPS float64         `json:"receive_bitrate_bps"`
	CatalogBitrateBPS *int            `json:"catalog_bitrate_bps"`
	BufferLevelMS     *float64        `json:"buffer_level_ms"`
	PlaybackRate      *float64        `json:"playback_rate"`
	StallCount        int             `json:"stall_count"`
	StallDurationMS   float64         `json:"stall_duration_ms"`
}

type nativePlayback struct {
	mu sync.Mutex

	tracks     []playbackTrack
	trackIndex map[string]int
	target     int
	active     int
	valid      []bool
	pending    map[playbackUnitKey]*playbackUnit

	simulated       bool
	minimalBuffer   time.Duration
	targetLatency   time.Duration
	metricsPath     string
	state           string
	switchState     string
	started         bool
	lastPresented   *uint64
	lastPresentedAt time.Time
	stallStarted    time.Time
	stallCount      int
	stallDuration   time.Duration
	received        []timedBytes
	played          []timedBytes
	wake            chan struct{}
}

func newNativePlayback(catalog *internal.Catalog, names []string, targetName string,
	simulated bool, minimalBuffer, targetLatency time.Duration, metricsPath string,
) (*nativePlayback, error) {
	if len(names) == 0 {
		return nil, fmt.Errorf("empty video dependency chain")
	}
	p := &nativePlayback{
		trackIndex:    make(map[string]int, len(names)),
		target:        len(names) - 1,
		active:        -1,
		valid:         make([]bool, len(names)),
		pending:       make(map[playbackUnitKey]*playbackUnit),
		simulated:     simulated,
		minimalBuffer: minimalBuffer,
		targetLatency: targetLatency,
		metricsPath:   metricsPath,
		state:         "receiving",
		switchState:   "stable",
		wake:          make(chan struct{}, 1),
	}
	if simulated {
		p.state = "starting"
		if minimalBuffer < 0 || targetLatency <= minimalBuffer {
			return nil, fmt.Errorf("target latency must exceed non-negative minimum buffer")
		}
	}
	for i, name := range names {
		track := catalog.GetTrackByName(name)
		if track == nil {
			return nil, fmt.Errorf("video dependency track %q not found", name)
		}
		if simulated && (track.Packaging != "loc" || !strings.HasPrefix(strings.ToLower(track.Codec), "av01")) {
			return nil, fmt.Errorf("simulated playback requires LOC AV1 track %q", name)
		}
		spatialID := i
		if track.SpatialID != nil {
			spatialID = *track.SpatialID
		}
		bitrate := 0
		if track.Bitrate != nil {
			bitrate = *track.Bitrate
		}
		p.tracks = append(p.tracks, playbackTrack{
			name: name, spatialID: spatialID, width: track.Width,
			height: track.Height, bitrate: bitrate,
		})
		p.trackIndex[name] = i
	}
	if p.tracks[p.target].name != targetName {
		return nil, fmt.Errorf("target track %q is not the dependency-chain tip", targetName)
	}
	return p, nil
}

func (p *nativePlayback) run(ctx context.Context) {
	metricsTicker := time.NewTicker(time.Second)
	advanceTimer := time.NewTimer(time.Hour)
	if !advanceTimer.Stop() {
		<-advanceTimer.C
	}
	var advanceC <-chan time.Time
	schedule := func() {
		if !advanceTimer.Stop() {
			select {
			case <-advanceTimer.C:
			default:
			}
		}
		advanceC = nil
		deadline, ok := p.nextAdvanceDeadline()
		if !ok {
			return
		}
		delay := time.Until(deadline)
		if delay < 0 {
			delay = 0
		}
		advanceTimer.Reset(delay)
		advanceC = advanceTimer.C
	}
	defer advanceTimer.Stop()
	defer metricsTicker.Stop()
	p.writeMetrics(time.Now())
	schedule()
	for {
		select {
		case deadline := <-advanceC:
			advanceC = nil
			p.advance(deadline)
			schedule()
		case <-p.wake:
			p.advance(time.Now())
			schedule()
		case now := <-metricsTicker.C:
			p.writeMetrics(now)
		case <-ctx.Done():
			p.writeMetrics(time.Now())
			return
		}
	}
}

func (p *nativePlayback) addObject(trackName string, group, object uint64,
	headers moqtransport.KVPList, size int, now time.Time,
) {
	p.mu.Lock()
	notify := false
	defer func() {
		p.mu.Unlock()
		if notify {
			p.notifyPlayback()
		}
	}()
	p.received = append(p.received, timedBytes{at: now, bytes: size})
	if !p.simulated {
		return
	}
	notify = true
	index, ok := p.trackIndex[trackName]
	if !ok {
		return
	}
	timestampUs, hasTimestamp := locTimestampMicros(headers)
	marking, hasMarking, err := getLOCFrameMarking(headers)
	if err != nil || !hasTimestamp || !hasMarking || !validPlaybackMarking(marking, p.tracks[index].spatialID) {
		slog.Debug("dropping invalid simulated playback layer", "track", trackName,
			"groupID", group, "objectID", object)
		return
	}
	key := playbackUnitKey{group: group, object: object}
	unit := p.pending[key]
	if unit == nil {
		unit = &playbackUnit{key: key, layers: make(map[int]playbackLayer), invalidFrom: len(p.tracks)}
		p.pending[key] = unit
	}
	layer := playbackLayer{timestampUs: timestampUs, independent: marking.Independent, bytes: size}
	if previous, exists := unit.layers[index]; exists {
		if previous != layer && index < unit.invalidFrom {
			unit.invalidFrom = index
		}
	} else {
		unit.layers[index] = layer
	}
	p.trimPending()
}

func (p *nativePlayback) notifyPlayback() {
	select {
	case p.wake <- struct{}{}:
	default:
	}
}

func validPlaybackMarking(marking locFrameMarking, spatialID int) bool {
	return marking.Start && marking.End && !marking.Discardable &&
		!marking.BaseLayerSync && marking.TemporalID == 0 &&
		int(marking.LayerID) == spatialID
}

func (p *nativePlayback) advance(now time.Time) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if !p.simulated {
		return
	}
	if p.active < 0 {
		p.tryStart(now)
		return
	}
	for {
		unit := p.nextUnit()
		if unit == nil {
			return
		}
		timestampUs, ok := unit.timestamp()
		if !ok {
			return
		}
		deadline := p.presentationDeadline(timestampUs)
		if now.Before(deadline) {
			return
		}
		prefix, independent := p.completePrefix(unit)
		if prefix < p.active {
			for i := prefix + 1; i < len(p.valid); i++ {
				p.valid[i] = false
			}
		}
		if independent {
			for i := 0; i <= prefix; i++ {
				p.valid[i] = true
			}
			p.active = prefix
		} else {
			for p.active >= 0 && (p.active > prefix || !p.valid[p.active]) {
				p.active--
			}
		}
		if p.active < 0 {
			p.enterStall(now)
			p.switchState = "waiting_independent"
			return
		}
		p.present(unit, now)
	}
}

func (p *nativePlayback) tryStart(now time.Time) {
	for _, unit := range p.sortedUnits() {
		prefix, independent := p.completePrefix(unit)
		if !independent || prefix < 0 || p.bufferFor(unit, prefix) < p.minimalBuffer {
			continue
		}
		base := unit.layers[0]
		deadline := time.UnixMicro(int64(base.timestampUs)).Add(p.targetLatency)
		if now.Before(deadline) {
			p.state = "buffering"
			return
		}
		for i := 0; i <= prefix; i++ {
			p.valid[i] = true
		}
		p.active = prefix
		p.leaveStall(now)
		p.present(unit, now)
		return
	}
	if p.started {
		p.enterStall(now)
		p.switchState = "waiting_independent"
	} else {
		p.state = "buffering"
	}
}

func (p *nativePlayback) completePrefix(unit *playbackUnit) (int, bool) {
	if unit.invalidFrom == 0 {
		return -1, false
	}
	base, ok := unit.layers[0]
	if !ok {
		return -1, false
	}
	prefix := -1
	for i := 0; i < len(p.tracks) && i < unit.invalidFrom; i++ {
		layer, exists := unit.layers[i]
		if !exists || layer.timestampUs != base.timestampUs || layer.independent != base.independent {
			break
		}
		prefix = i
	}
	return prefix, base.independent
}

func (p *nativePlayback) bufferFor(first *playbackUnit, prefix int) time.Duration {
	start := first.layers[0].timestampUs
	latest := start
	seenFirst := false
	for _, unit := range p.sortedUnits() {
		timestamp, hasTimestamp := unit.timestamp()
		if !hasTimestamp || timestamp < start {
			continue
		}
		base, ok := unit.layers[0]
		if unit == first {
			seenFirst = true
		}
		if !seenFirst {
			continue
		}
		if !ok {
			break
		}
		complete, _ := p.completePrefix(unit)
		if complete < prefix {
			break
		}
		latest = base.timestampUs
	}
	return time.Duration(latest-start) * time.Microsecond
}

func (p *nativePlayback) present(unit *playbackUnit, now time.Time) {
	base := unit.layers[0]
	bytes := 0
	for i := 0; i <= p.active; i++ {
		bytes += unit.layers[i].bytes
	}
	p.played = append(p.played, timedBytes{at: now, bytes: bytes})
	p.lastPresented = &base.timestampUs
	p.lastPresentedAt = now
	delete(p.pending, unit.key)
	p.started = true
	p.state = "playing"
	if p.active < p.target {
		p.switchState = "waiting_independent"
	} else {
		p.switchState = "stable"
	}
}

func (p *nativePlayback) presentationDeadline(timestampUs uint64) time.Time {
	if !p.started || p.lastPresented == nil || p.lastPresentedAt.IsZero() {
		return time.UnixMicro(int64(timestampUs)).Add(p.targetLatency)
	}
	if timestampUs <= *p.lastPresented {
		return p.lastPresentedAt
	}
	return p.lastPresentedAt.Add(time.Duration(timestampUs-*p.lastPresented) * time.Microsecond)
}

func (p *nativePlayback) nextAdvanceDeadline() (time.Time, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if !p.simulated {
		return time.Time{}, false
	}
	if p.active >= 0 {
		unit := p.nextUnit()
		if unit == nil {
			return time.Time{}, false
		}
		timestampUs, ok := unit.timestamp()
		if !ok {
			return time.Time{}, false
		}
		return p.presentationDeadline(timestampUs), true
	}
	for _, unit := range p.sortedUnits() {
		prefix, independent := p.completePrefix(unit)
		if !independent || prefix < 0 || p.bufferFor(unit, prefix) < p.minimalBuffer {
			continue
		}
		base := unit.layers[0]
		return time.UnixMicro(int64(base.timestampUs)).Add(p.targetLatency), true
	}
	return time.Time{}, false
}

func (p *nativePlayback) enterStall(now time.Time) {
	if p.state == "stalled" {
		return
	}
	p.state = "stalled"
	p.stallCount++
	p.stallStarted = now
}

func (p *nativePlayback) leaveStall(now time.Time) {
	if p.stallStarted.IsZero() {
		return
	}
	p.stallDuration += now.Sub(p.stallStarted)
	p.stallStarted = time.Time{}
}

func (p *nativePlayback) nextUnit() *playbackUnit {
	for _, unit := range p.sortedUnits() {
		timestampUs, ok := unit.timestamp()
		if !ok {
			continue
		}
		if p.lastPresented == nil || timestampUs > *p.lastPresented {
			return unit
		}
		delete(p.pending, unit.key)
	}
	return nil
}

func (p *nativePlayback) sortedUnits() []*playbackUnit {
	units := make([]*playbackUnit, 0, len(p.pending))
	for _, unit := range p.pending {
		units = append(units, unit)
	}
	// ponytail: pending is capped at 64; replace this scan only if that ceiling grows.
	sort.Slice(units, func(i, j int) bool {
		a, aok := units[i].timestamp()
		b, bok := units[j].timestamp()
		if aok && bok && a != b {
			return a < b
		}
		if aok != bok {
			return aok
		}
		if units[i].key.group != units[j].key.group {
			return units[i].key.group < units[j].key.group
		}
		return units[i].key.object < units[j].key.object
	})
	return units
}

func (u *playbackUnit) timestamp() (uint64, bool) {
	lowest := 0
	var timestamp uint64
	found := false
	for index, layer := range u.layers {
		if !found || index < lowest {
			lowest = index
			timestamp = layer.timestampUs
			found = true
		}
	}
	return timestamp, found
}

func (p *nativePlayback) trimPending() {
	for len(p.pending) > maxPendingUnits {
		units := p.sortedUnits()
		delete(p.pending, units[0].key)
	}
}

func (p *nativePlayback) snapshot(now time.Time) playbackMetrics {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.received = trimByteWindow(p.received, now)
	p.played = trimByteWindow(p.played, now)
	target := p.tracks[p.target].name
	metrics := playbackMetrics{
		SchemaVersion: metricsSchemaVersion, SampledAtUnixMS: now.UnixMilli(),
		Client: "native", Simulated: p.simulated, State: p.state,
		TargetTrack: &target, SwitchState: p.switchState,
		Quality: playbackQuality{}, ReceiveBitrateBPS: byteRate(p.received),
		StallCount: p.stallCount, StallDurationMS: float64(p.stallDuration) / float64(time.Millisecond),
	}
	if !p.stallStarted.IsZero() {
		metrics.StallDurationMS += float64(now.Sub(p.stallStarted)) / float64(time.Millisecond)
	}
	if !p.simulated {
		return metrics
	}
	rate := 1.0
	metrics.PlaybackRate = &rate
	playerRate := byteRate(p.played)
	metrics.PlayerBitrateBPS = &playerRate
	if p.active >= 0 {
		track := p.tracks[p.active]
		metrics.ActiveTrack = &track.name
		spatialID := track.spatialID
		metrics.Quality = playbackQuality{SpatialID: &spatialID, Width: track.width, Height: track.height}
		catalogRate := 0
		for i := 0; i <= p.active; i++ {
			catalogRate += p.tracks[i].bitrate
		}
		metrics.CatalogBitrateBPS = &catalogRate
		bufferMS := float64(p.bufferAhead(now)) / float64(time.Millisecond)
		metrics.BufferLevelMS = &bufferMS
	}
	if p.lastPresented != nil {
		latency := float64(now.UnixMicro()-int64(*p.lastPresented)) / 1000
		if latency >= 0 {
			metrics.E2ELatencyMS = &latency
		}
	}
	return metrics
}

func (p *nativePlayback) bufferAhead(now time.Time) time.Duration {
	if p.active < 0 || p.lastPresented == nil {
		return 0
	}
	latest := *p.lastPresented
	for _, unit := range p.sortedUnits() {
		timestamp, hasTimestamp := unit.timestamp()
		if !hasTimestamp || timestamp <= latest {
			continue
		}
		base, ok := unit.layers[0]
		if !ok {
			break
		}
		prefix, _ := p.completePrefix(unit)
		if prefix < p.active {
			break
		}
		latest = base.timestampUs
	}
	playhead := p.playheadTimestamp(now)
	if latest <= playhead {
		return 0
	}
	return time.Duration(latest-playhead) * time.Microsecond
}

func (p *nativePlayback) playheadTimestamp(now time.Time) uint64 {
	if p.lastPresented == nil || p.lastPresentedAt.IsZero() {
		return 0
	}
	until := now
	if !p.stallStarted.IsZero() && p.stallStarted.Before(until) {
		until = p.stallStarted
	}
	elapsed := until.Sub(p.lastPresentedAt)
	if elapsed <= 0 {
		return *p.lastPresented
	}
	return *p.lastPresented + uint64(elapsed.Microseconds())
}

func trimByteWindow(samples []timedBytes, now time.Time) []timedBytes {
	cutoff := now.Add(-time.Second)
	first := 0
	for first < len(samples) && samples[first].at.Before(cutoff) {
		first++
	}
	return samples[first:]
}

func byteRate(samples []timedBytes) float64 {
	bytes := 0
	for _, sample := range samples {
		bytes += sample.bytes
	}
	return float64(bytes * 8)
}

func (p *nativePlayback) writeMetrics(now time.Time) {
	if p.metricsPath == "" {
		return
	}
	payload, err := json.Marshal(p.snapshot(now))
	if err != nil {
		slog.Error("failed to marshal native playback metrics", "error", err)
		return
	}
	temporary := p.metricsPath + ".tmp"
	if err := os.WriteFile(temporary, append(payload, '\n'), 0o644); err != nil {
		slog.Error("failed to write native playback metrics", "error", err)
		return
	}
	if err := os.Rename(temporary, p.metricsPath); err != nil {
		slog.Error("failed to publish native playback metrics", "error", err)
	}
}
