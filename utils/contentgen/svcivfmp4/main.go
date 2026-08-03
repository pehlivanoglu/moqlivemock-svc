// Command svcivfmp4 converts the combined IVF output from libaom's
// svc_encoder_rtc into a fragmented MP4 with one temporal unit per sample.
package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"sort"

	"github.com/Eyevinn/mp4ff/av1"
	"github.com/Eyevinn/mp4ff/ivf"
	"github.com/Eyevinn/mp4ff/mp4"
)

const (
	timeScale = 12800
	sampleDur = 512 // 25 fps
)

func main() {
	if len(os.Args) != 3 {
		fmt.Fprintf(os.Stderr, "usage: %s input.ivf output.mp4\n", os.Args[0])
		os.Exit(2)
	}
	if err := run(os.Args[1], os.Args[2]); err != nil {
		fmt.Fprintf(os.Stderr, "svcivfmp4: %v\n", err)
		os.Exit(1)
	}
}

func run(inputPath, outputPath string) error {
	in, err := os.Open(inputPath)
	if err != nil {
		return err
	}
	defer in.Close()

	reader, err := ivf.NewReader(in)
	if err != nil {
		return err
	}
	if reader.Header.FourCC != ivf.CodecAV1 {
		return fmt.Errorf("input codec is %q, want AV1", reader.Header.FourCC)
	}
	if reader.Header.Rate != 25 || reader.Header.Scale != 1 {
		return fmt.Errorf("input rate is %d/%d, want 25/1", reader.Header.Rate, reader.Header.Scale)
	}

	var packets []ivf.Frame
	for {
		frame, err := reader.ReadFrame()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return err
		}
		packets = append(packets, frame)
	}
	frames, err := combineSpatialPackets(packets)
	if err != nil {
		return err
	}
	if len(frames) == 0 {
		return fmt.Errorf("no frames")
	}

	sequence, err := findOBU(frames[0].Data, av1.OBUSequenceHeader)
	if err != nil {
		return err
	}
	sh, err := av1.ParseSequenceHeader(sequence.Payload)
	if err != nil {
		return fmt.Errorf("parse sequence header: %w", err)
	}
	av1C := &mp4.Av1CBox{CodecConfRec: av1.CodecConfRecFromSequenceHeader(sh, sequence.Encode())}
	init := mp4.CreateEmptyInit()
	trak := init.AddEmptyTrack(timeScale, "video", "und")
	if err := trak.SetAV1Descriptor("av01", av1C, uint16(sh.Width()), uint16(sh.Height())); err != nil {
		return err
	}

	out, err := os.Create(outputPath)
	if err != nil {
		return err
	}
	defer out.Close()
	if err := init.Encode(out); err != nil {
		return err
	}
	trackID := init.Moov.Trak.Tkhd.TrackID
	for i := range frames {
		isKey, err := av1.IsRAPSample(frames[i].Data, sh)
		if err != nil {
			return fmt.Errorf("frame %d: determine sync status: %w", i, err)
		}
		flags := mp4.SetNonSyncSampleFlags(0)
		if isKey {
			flags = mp4.SetSyncSampleFlags(0)
		}
		fragment, err := mp4.CreateFragment(uint32(i+1), trackID)
		if err != nil {
			return err
		}
		fragment.AddFullSample(mp4.FullSample{
			Sample:     mp4.Sample{Flags: flags, Dur: sampleDur, Size: uint32(len(frames[i].Data))},
			DecodeTime: uint64(i * sampleDur),
			Data:       frames[i].Data,
		})
		if err := fragment.Encode(out); err != nil {
			return fmt.Errorf("encode fragment %d: %w", i+1, err)
		}
	}
	return nil
}

func combineSpatialPackets(packets []ivf.Frame) ([]ivf.Frame, error) {
	type packet struct {
		spatialID byte
		obus      []av1.OBU
	}
	groups := make(map[uint64][]packet)
	var timestamps []uint64
	for packetNr, frame := range packets {
		obus, err := av1.SplitOBUs(frame.Data)
		if err != nil {
			return nil, fmt.Errorf("packet %d: %w", packetNr, err)
		}
		spatialID, err := packetSpatialID(obus)
		if err != nil {
			return nil, fmt.Errorf("packet %d: %w", packetNr, err)
		}
		if _, exists := groups[frame.Timestamp]; !exists {
			timestamps = append(timestamps, frame.Timestamp)
		}
		for _, existing := range groups[frame.Timestamp] {
			if existing.spatialID == spatialID {
				return nil, fmt.Errorf("timestamp %d has duplicate spatial ID %d", frame.Timestamp, spatialID)
			}
		}
		groups[frame.Timestamp] = append(groups[frame.Timestamp], packet{spatialID: spatialID, obus: obus})
	}
	sort.Slice(timestamps, func(i, j int) bool { return timestamps[i] < timestamps[j] })

	frames := make([]ivf.Frame, 0, len(timestamps))
	expectedLayers := 0
	for _, timestamp := range timestamps {
		group := groups[timestamp]
		sort.Slice(group, func(i, j int) bool { return group[i].spatialID < group[j].spatialID })
		if expectedLayers == 0 {
			expectedLayers = len(group)
		} else if len(group) != expectedLayers {
			return nil, fmt.Errorf("timestamp %d has %d spatial layers, want %d",
				timestamp, len(group), expectedLayers)
		}
		for i := range group {
			if int(group[i].spatialID) != i {
				return nil, fmt.Errorf("timestamp %d: missing spatial ID %d", timestamp, i)
			}
		}
		seenGlobal := make(map[av1.OBUType]bool)
		var data []byte
		for _, layer := range group {
			for _, obu := range layer.obus {
				if !obu.Header.ExtensionFlag && (obu.Header.Type == av1.OBUSequenceHeader ||
					obu.Header.Type == av1.OBUTemporalDelimiter) {
					if seenGlobal[obu.Header.Type] {
						continue
					}
					seenGlobal[obu.Header.Type] = true
				}
				data = append(data, obu.Encode()...)
			}
		}
		frames = append(frames, ivf.Frame{Timestamp: timestamp, Data: data})
	}
	return frames, nil
}

func packetSpatialID(obus []av1.OBU) (byte, error) {
	for _, obu := range obus {
		if obu.Header.Type == av1.OBUFrame || obu.Header.Type == av1.OBUFrameHeader {
			return obu.Header.SpatialID, nil
		}
	}
	return 0, fmt.Errorf("packet contains no frame OBU")
}

func findOBU(data []byte, typ av1.OBUType) (av1.OBU, error) {
	obus, err := av1.SplitOBUs(data)
	if err != nil {
		return av1.OBU{}, err
	}
	for _, obu := range obus {
		if obu.Header.Type == typ {
			return obu, nil
		}
	}
	return av1.OBU{}, fmt.Errorf("no %s OBU", typ)
}
