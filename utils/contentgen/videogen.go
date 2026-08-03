package main

import (
	"flag"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

const (
	duration    = 10 // seconds
	frameRate   = 25 // fps
	outputDir   = "output"
	logDir      = "logs"
	videoWidth  = 1280
	videoHeight = 720
)

// getFFmpegPath returns the path to ffmpeg, checking FFMPEG_PATH env var first
func getFFmpegPath() string {
	if path := os.Getenv("FFMPEG_PATH"); path != "" {
		return path
	}
	return "ffmpeg"
}

func getSVCEncoderPath() string {
	if path := os.Getenv("SVC_ENCODER_PATH"); path != "" {
		return path
	}
	return "svc_encoder_rtc"
}

func main() {
	// Parse command line flags
	codecList := flag.String("codecs", "h264", "Comma-separated list of video codecs to generate (h264,h265,av1,svc)")
	fragmentDuration := flag.Int("fragment-duration", 0, "Fragment duration in milliseconds (0 = one sample/fragment)")
	flag.Parse()

	// Parse the codec list
	codecs := strings.Split(*codecList, ",")
	codecMap := make(map[string]bool)
	for _, codec := range codecs {
		codecMap[strings.TrimSpace(codec)] = true
	}

	// Create output directory if it doesn't exist
	if err := os.MkdirAll(outputDir, 0755); err != nil {
		log.Fatalf("Failed to create output directory: %v", err)
	}

	// Create logs directory if it doesn't exist
	if err := os.MkdirAll(logDir, 0755); err != nil {
		log.Fatalf("Failed to create logs directory: %v", err)
	}

	// Check and prepare required files
	ensureRequiredFiles()

	type videoSetup struct {
		codec   string
		options []string
	}

	setups := []videoSetup{
		{"h264", []string{
			"-c:v", "libx264",
			"-preset", "medium",
			"-profile:v", "main",
			"-x264opts", fmt.Sprintf("keyint=%d:min-keyint=%d:scenecut=0:bframes=0:force-cfr=1", frameRate, frameRate),
			"-pix_fmt", "yuv420p"},
		},
		{"h265", []string{
			"-c:v", "libx265",
			"-preset", "medium",
			"-x265-params", fmt.Sprintf(
				"profile=main:keyint=%d:min-keyint=%d:scenecut=0:bframes=0:open-gop=0", frameRate, frameRate),
			"-pix_fmt", "yuv420p",
			"-tag:v", "hvc1"},
		},
		{"av1", []string{
			"-c:v", "libsvtav1",
			"-preset", "6",
			// Low-delay (pred-struct=1) gives I/P frames only (no reordering,
			// no composition offsets), matching the AVC/HEVC structure. SVT-AV1
			// requires CBR (rc=2) for low-delay; scd=0 disables scene-cut so the
			// keyframe cadence stays fixed at one IDR per second.
			"-svtav1-params", fmt.Sprintf("keyint=%d:scd=0:pred-struct=1:rc=2", frameRate),
			"-pix_fmt", "yuv420p"},
		},
	}

	// Generate video files based on codec selection
	for _, setup := range setups {
		if codecMap[setup.codec] {
			// Generate video files at different bitrates
			videoBitrates := []int{400, 600, 900} // kbps
			for _, bitrate := range videoBitrates {
				generateVideo(setup.codec, setup.options, bitrate, *fragmentDuration)
			}
		}
	}
	if codecMap["svc"] {
		if *fragmentDuration != 0 {
			log.Fatal("AV1 SVC generation requires -fragment-duration=0 (one temporal unit per fragment)")
		}
		generateSVCVideo()
	}

	fmt.Println("All video files generated successfully!")

	// Print average bitrates based on file sizes
	printActualBitrates(codecMap)
}

func ensureRequiredFiles() {
	// Check if font file exists
	if _, err := os.Stat("resources/RobotoSlab-Regular.ttf"); os.IsNotExist(err) {
		log.Fatalf("Required font file resources/RobotoSlab-Regular.ttf not found")
	}
}

func generateVideo(codec string, options []string, bitrateKbps, fragmentDurationMs int) {
	// Map internal codec names to output file codec suffixes and the
	// human-readable label burned into the first text line of the video.
	codecSuffix := codec
	codecLabel := strings.ToUpper(codec)
	switch codec {
	case "h264":
		codecSuffix = "avc"
		codecLabel = "AVC"
	case "h265":
		codecSuffix = "hevc"
		codecLabel = "HEVC"
	case "av1":
		codecSuffix = "av1"
		codecLabel = "AV1"
	}
	outputFile := filepath.Join(outputDir, fmt.Sprintf("video_%dkbps_%s.mp4", bitrateKbps, codecSuffix))
	logFile := filepath.Join(logDir, fmt.Sprintf("video_%dkbps_%s.log", bitrateKbps, codecSuffix))
	fmt.Printf("Generating video file at %d kbps: %s\n", bitrateKbps, outputFile)

	// Create log file
	logFileHandle, err := os.Create(logFile)
	if err != nil {
		log.Fatalf("Failed to create log file: %v", err)
	}
	defer logFileHandle.Close()

	logoFile := "resources/logo.png"

	// Select text color based on bitrate
	var textColor string
	switch bitrateKbps {
	case 400:
		textColor = "white"
	case 600:
		textColor = "yellow"
	case 900:
		textColor = "orange"
	default:
		textColor = "white"
	}

	videoFilter := buildVideoFilter(codecLabel, fmt.Sprintf("%d kbps", bitrateKbps),
		fmt.Sprintf("%d x %d", videoWidth, videoHeight), textColor)

	// ffmpeg command line args
	cmdArgsFirst := []string{
		"-y",
		"-f", "lavfi",
		"-i", fmt.Sprintf("testsrc=size=%dx%d:rate=%d:duration=%d:decimals=3", videoWidth, videoHeight, frameRate, duration),
		"-loop", "1", // Loop the logo image
		"-framerate", fmt.Sprintf("%d", frameRate), // Match video framerate
		"-i", logoFile,
		"-filter_complex", videoFilter,
	}

	// Build movflags based on fragment duration
	var movflags string
	if fragmentDurationMs == 0 {
		movflags = "cmaf+separate_moof+delay_moov+skip_trailer+frag_every_frame"
	} else {
		movflags = "cmaf+separate_moof+delay_moov+skip_trailer"
	}

	cmdArgsLast := []string{
		"-b:v", fmt.Sprintf("%dk", bitrateKbps),
		"-an",
		"-movflags", movflags,
	}

	// Add fragment duration if specified
	if fragmentDurationMs > 0 {
		fragmentDurationMicros := fragmentDurationMs * 1000 // Convert ms to microseconds
		cmdArgsLast = append(cmdArgsLast, "-frag_duration", fmt.Sprintf("%d", fragmentDurationMicros))
	}

	cmdArgsLast = append(cmdArgsLast, outputFile)

	// Print the ffmpeg command
	cmdArgs := make([]string, 0, len(cmdArgsFirst)+len(options)+len(cmdArgsLast))
	cmdArgs = append(cmdArgs, cmdArgsFirst...)
	cmdArgs = append(cmdArgs, options...)
	cmdArgs = append(cmdArgs, cmdArgsLast...)
	ffmpegPath := getFFmpegPath()
	cmdString := ffmpegPath + " " + strings.Join(cmdArgs, " ")
	fmt.Println("Executing ffmpeg command:")
	fmt.Println(cmdString)

	// Write command to log file
	_, _ = logFileHandle.WriteString("Command: " + cmdString + "\n\n")

	// Run ffmpeg command
	cmd := exec.Command(ffmpegPath, cmdArgs...)
	cmd.Stdout = logFileHandle
	cmd.Stderr = logFileHandle

	if err := cmd.Run(); err != nil {
		log.Fatalf("Failed to generate video file at %d kbps: %v", bitrateKbps, err)
	}

	fmt.Printf("Video generation completed. Log saved to: %s\n", logFile)
}

func buildVideoFilter(codecLabel, bitrateLabel, resolutionLabel, textColor string) string {
	fontFile := "resources/RobotoSlab-Regular.ttf"
	// Scale logo to half size, then rotate so it completes a full turn in 10s.
	logoScale := "scale=iw/2:ih/2"
	rotationDuration := float64(duration) // 10s for a full rotation
	rotationExpr := fmt.Sprintf("2*PI*n/(%d*%d)", frameRate, int(rotationDuration))
	//nolint: lll
	videoFilter := fmt.Sprintf(
		"[1:v]%s,format=rgba,rotate='%s':c=none:ow=rotw(iw):oh=roth(ih)[logo];"+
			"[0:v][logo]overlay=x=20:y=main_h-overlay_h-20:shortest=1[bg];"+
			"[bg]drawtext=fontfile=%s:text='Codec\\: %s':fontcolor=%s:fontsize=36:box=1:boxcolor=black@0.5:boxborderw=5:x=20:y=20,"+
			"drawtext=fontfile=%s:text='Bitrate\\: %s':fontcolor=%s:fontsize=36:box=1:boxcolor=black@0.5:boxborderw=5:x=20:y=70,"+
			"drawtext=fontfile=%s:text='Resolution\\: %s':fontcolor=%s:fontsize=36:box=1:boxcolor=black@0.5:boxborderw=5:x=20:y=120,"+
			"drawtext=fontfile=%s:text='Time\\: %%{pts\\:hms}':fontcolor=%s:fontsize=36:box=1:boxcolor=black@0.5:boxborderw=5:x=20:y=170,"+
			"drawtext=fontfile=%s:text='Frame\\: %%{frame_num}':fontcolor=%s:fontsize=36:box=1:boxcolor=black@0.5:boxborderw=5:x=20:y=220",
		logoScale, rotationExpr,
		fontFile, codecLabel, textColor, fontFile, bitrateLabel, textColor, fontFile, resolutionLabel, textColor, fontFile, textColor, fontFile, textColor,
	)
	return videoFilter
}

func generateSVCVideo() {
	outputFile := filepath.Join(outputDir, "video.mp4")
	y4mFile := filepath.Join(outputDir, "video_svc_input.y4m")
	ivfFile := filepath.Join(outputDir, "video_svc.ivf")
	logFile := filepath.Join(logDir, "video_svc.log")
	logFileHandle, err := os.Create(logFile)
	if err != nil {
		log.Fatalf("Failed to create SVC log file: %v", err)
	}
	defer logFileHandle.Close()
	defer os.Remove(y4mFile)
	defer os.Remove(ivfFile)
	for spatialID := range 3 {
		defer os.Remove(fmt.Sprintf("%s_%d.av1", ivfFile, spatialID))
	}

	filter := buildVideoFilter("AV1 SVC", "150 / 450 / 900 kbps cumulative",
		"320x180 / 640x360 / 1280x720", "white")
	ffmpegArgs := []string{
		"-y",
		"-f", "lavfi",
		"-i", fmt.Sprintf("testsrc=size=%dx%d:rate=%d:duration=%d:decimals=3", videoWidth, videoHeight, frameRate, duration),
		"-loop", "1", "-framerate", fmt.Sprintf("%d", frameRate), "-i", "resources/logo.png",
		"-filter_complex", filter,
		"-pix_fmt", "yuv420p", "-an", "-f", "yuv4mpegpipe", y4mFile,
	}
	if err := runLogged(logFileHandle, getFFmpegPath(), ffmpegArgs...); err != nil {
		log.Fatalf("Failed to generate SVC Y4M input: %v", err)
	}

	encoderArgs := []string{
		"-w", "1280", "-h", "720", "-t", "1/25", "-b", "900",
		"-sl", "3", "-tl", "1", "-lm", "6", "-k", "25",
		"-r", "1/4,1/2,1/1", "-bl", "150,300,450",
		"--min-q=2", "--max-q=56", "-sp", "10", "-th", "4",
		"--output-obu=0", "--test-decode=1", y4mFile, "-o", ivfFile,
	}
	if err := runLogged(logFileHandle, getSVCEncoderPath(), encoderArgs...); err != nil {
		log.Fatalf("Failed to encode AV1 SVC video: %v", err)
	}

	if err := runLogged(logFileHandle, "go", "run", "./svcivfmp4", ivfFile, outputFile); err != nil {
		log.Fatalf("Failed to package AV1 SVC video: %v", err)
	}
	fmt.Printf("AV1 SVC video generation completed: %s (log: %s)\n", outputFile, logFile)
}

func runLogged(logFile *os.File, command string, args ...string) error {
	cmdString := command + " " + strings.Join(args, " ")
	fmt.Println(cmdString)
	_, _ = logFile.WriteString("Command: " + cmdString + "\n\n")
	cmd := exec.Command(command, args...)
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	return cmd.Run()
}

func printActualBitrates(codecMap map[string]bool) {
	fmt.Println("\nActual average bitrates based on file sizes:")
	fmt.Println("--------------------------------------------")

	// Check video files based on selected codecs
	videoBitrates := []int{400, 600, 900} // kbps - keep in sync with main()
	for _, bitrate := range videoBitrates {
		if codecMap["h264"] {
			videoFile := filepath.Join(outputDir, fmt.Sprintf("video_%dkbps_avc.mp4", bitrate))
			printFileBitrate(videoFile, duration)
		}
		if codecMap["h265"] {
			videoFile := filepath.Join(outputDir, fmt.Sprintf("video_%dkbps_hevc.mp4", bitrate))
			printFileBitrate(videoFile, duration)
		}
		if codecMap["av1"] {
			videoFile := filepath.Join(outputDir, fmt.Sprintf("video_%dkbps_av1.mp4", bitrate))
			printFileBitrate(videoFile, duration)
		}
	}
	if codecMap["svc"] {
		printFileBitrate(filepath.Join(outputDir, "video.mp4"), duration)
	}
}

func printFileBitrate(filePath string, durationSec int) {
	fileInfo, err := os.Stat(filePath)
	if err != nil {
		fmt.Printf("Error getting file info for %s: %v\n", filePath, err)
		return
	}

	// Calculate bitrate: (file size in bits) / (duration in seconds)
	fileSizeBytes := fileInfo.Size()
	fileSizeBits := fileSizeBytes * 8
	actualBitrateKbps := float64(fileSizeBits) / float64(durationSec) / 1000.0

	// Get target bitrate from filename
	fileName := filepath.Base(filePath)

	fmt.Printf("Video file: %s\n", fileName)
	fmt.Printf("  File size: %.2f KB (%.2f MB)\n", float64(fileSizeBytes)/1024.0, float64(fileSizeBytes)/1024.0/1024.0)
	fmt.Printf("  Duration: %d seconds\n", durationSec)
	fmt.Printf("  Average bitrate: %.2f kbps\n\n", actualBitrateKbps)
}
