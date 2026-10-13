package doubaoime

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"client2api/internal/core"
)

type wavAudio struct {
	samples       []int16
	sampleRate    int
	channels      int
	bitsPerSample int
}

func decodeAudio(ctx context.Context, ffmpeg string, raw []byte) (wavAudio, error) {
	if len(raw) >= 12 && string(raw[0:4]) == "RIFF" && string(raw[8:12]) == "WAVE" {
		return decodeWAV(raw)
	}
	_, ok := detectAudioFormat(raw)
	if !ok {
		return wavAudio{}, fmt.Errorf("%w: unrecognised audio container", core.ErrInvalidAudio)
	}
	if ffmpeg == "" {
		return wavAudio{}, core.Fail("doubaoime", "", core.FailureOther, http.StatusBadGateway,
			errors.New("doubaoime: this audio format needs an ffmpeg build with decoders for it; set ffmpeg_path"))
	}
	return decodeAudioWithFFmpeg(ctx, ffmpeg, raw)
}

func detectAudioFormat(raw []byte) (string, bool) {
	if len(raw) >= 4 && string(raw[0:4]) == "fLaC" {
		return "flac", true
	}
	if len(raw) >= 4 && string(raw[0:4]) == "OggS" {
		return "ogg", true
	}
	if len(raw) >= 4 && (string(raw[0:4]) == "ID3\x04" || string(raw[0:4]) == "ID3\x03" || (len(raw) >= 2 && raw[0] == 0xff && raw[1]&0xe0 == 0xe0)) {
		return "mp3", true
	}
	if len(raw) >= 12 && string(raw[4:8]) == "ftyp" {
		return "mp4", true
	}
	if len(raw) >= 4 && raw[0] == 0x1a && raw[1] == 0x45 && raw[2] == 0xdf && raw[3] == 0xa3 {
		return "webm", true
	}
	return "", false
}

func decodeAudioWithFFmpeg(ctx context.Context, ffmpeg string, raw []byte) (wavAudio, error) {
	format, ok := detectAudioFormat(raw)
	if !ok {
		return wavAudio{}, fmt.Errorf("%w: unrecognised audio container", core.ErrInvalidAudio)
	}

	// MP4/M4A commonly stores its "moov" atom at the end of the file, which
	// cannot be read from a non-seekable pipe. Give FFmpeg a real temporary
	// file so container probing works the same way it does for a normal
	// player. The extension is only a hint; FFmpeg still probes the bytes.
	file, err := os.CreateTemp("", "client2api-audio-*."+audioFileExtension(format))
	if err != nil {
		return wavAudio{}, core.Fail("doubaoime", "", core.FailureOther, http.StatusInternalServerError,
			fmt.Errorf("doubaoime: creating temporary audio file: %w", err))
	}
	path := file.Name()
	defer func() {
		_ = file.Close()
		_ = os.Remove(path)
	}()
	if _, err := file.Write(raw); err != nil {
		return wavAudio{}, core.Fail("doubaoime", "", core.FailureOther, http.StatusInternalServerError,
			fmt.Errorf("doubaoime: writing temporary audio file: %w", err))
	}
	if err := file.Close(); err != nil {
		return wavAudio{}, core.Fail("doubaoime", "", core.FailureOther, http.StatusInternalServerError,
			fmt.Errorf("doubaoime: closing temporary audio file: %w", err))
	}

	cmd := exec.CommandContext(ctx, ffmpeg,
		"-hide_banner", "-loglevel", "error",
		"-i", path,
		"-vn", "-f", "s16le", "-ar", "16000", "-ac", "1",
		"pipe:1",
	)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = err.Error()
		}
		return wavAudio{}, fmt.Errorf("%w: ffmpeg could not decode the audio: %s", core.ErrInvalidAudio, core.Redact(msg))
	}
	if len(out) < 2 {
		return wavAudio{}, fmt.Errorf("%w: ffmpeg produced no PCM audio", core.ErrInvalidAudio)
	}
	samples := make([]int16, len(out)/2)
	for i := range samples {
		samples[i] = int16(binary.LittleEndian.Uint16(out[i*2 : i*2+2]))
	}
	return wavAudio{samples: samples, sampleRate: 16000, channels: 1, bitsPerSample: 16}, nil
}

func audioFileExtension(format string) string {
	switch format {
	case "mp4":
		return "m4a"
	case "webm":
		return "webm"
	default:
		return format
	}
}

func decodeWAV(raw []byte) (wavAudio, error) {
	if len(raw) < 12 || string(raw[0:4]) != "RIFF" || string(raw[8:12]) != "WAVE" {
		return wavAudio{}, fmt.Errorf("%w: not a RIFF/WAVE file", core.ErrInvalidAudio)
	}
	var (
		out        wavAudio
		haveFormat bool
		data       []byte
	)
	for pos := 12; pos+8 <= len(raw); {
		id := string(raw[pos : pos+4])
		size := int(binary.LittleEndian.Uint32(raw[pos+4 : pos+8]))
		pos += 8
		if size < 0 || pos+size > len(raw) {
			return wavAudio{}, fmt.Errorf("%w: truncated %s chunk", core.ErrInvalidAudio, id)
		}
		chunk := raw[pos : pos+size]
		switch id {
		case "fmt ":
			if len(chunk) < 16 {
				return wavAudio{}, fmt.Errorf("%w: short fmt chunk", core.ErrInvalidAudio)
			}
			format := binary.LittleEndian.Uint16(chunk[0:2])
			if format != 1 {
				return wavAudio{}, fmt.Errorf("%w: only PCM WAV is supported", core.ErrInvalidAudio)
			}
			out.channels = int(binary.LittleEndian.Uint16(chunk[2:4]))
			out.sampleRate = int(binary.LittleEndian.Uint32(chunk[4:8]))
			out.bitsPerSample = int(binary.LittleEndian.Uint16(chunk[14:16]))
			haveFormat = true
		case "data":
			data = append([]byte(nil), chunk...)
		}
		pos += size
		if size%2 == 1 {
			pos++
		}
	}
	if !haveFormat || out.sampleRate <= 0 || out.channels <= 0 || out.bitsPerSample != 16 {
		return wavAudio{}, fmt.Errorf("%w: WAV must be 16-bit PCM", core.ErrInvalidAudio)
	}
	if len(data) == 0 || len(data)%2 != 0 {
		return wavAudio{}, fmt.Errorf("%w: WAV has no usable PCM data", core.ErrInvalidAudio)
	}
	out.samples = make([]int16, len(data)/2)
	for i := range out.samples {
		out.samples[i] = int16(binary.LittleEndian.Uint16(data[i*2 : i*2+2]))
	}
	return out, nil
}

func convertToPCM16k(samples []int16, sampleRate, channels int) []int16 {
	if sampleRate <= 0 || channels <= 0 || len(samples) == 0 {
		return nil
	}
	frames := len(samples) / channels
	mono := make([]int16, frames)
	for i := 0; i < frames; i++ {
		var sum int
		for ch := 0; ch < channels; ch++ {
			sum += int(samples[i*channels+ch])
		}
		mono[i] = int16(sum / channels)
	}
	if sampleRate == 16000 {
		return mono
	}
	outLen := int(math.Round(float64(len(mono)) * 16000 / float64(sampleRate)))
	if outLen <= 0 {
		return nil
	}
	out := make([]int16, outLen)
	ratio := float64(sampleRate) / 16000
	for i := range out {
		pos := float64(i) * ratio
		left := int(pos)
		if left >= len(mono)-1 {
			out[i] = mono[len(mono)-1]
			continue
		}
		frac := pos - float64(left)
		out[i] = int16(float64(mono[left])*(1-frac) + float64(mono[left+1])*frac)
	}
	return out
}

func isSilent(samples []int16) bool {
	if len(samples) == 0 {
		return true
	}
	var max, sumSquares float64
	for _, sample := range samples {
		value := math.Abs(float64(sample))
		if value > max {
			max = value
		}
		sumSquares += value * value
	}
	rms := math.Sqrt(sumSquares / float64(len(samples)))
	return max < 32 || rms < 8
}

func findFFmpeg(configured string) string {
	if configured = strings.TrimSpace(configured); configured != "" {
		if strings.ContainsAny(configured, `\/`) || filepath.IsAbs(configured) {
			if info, err := os.Stat(configured); err == nil && !info.IsDir() {
				return configured
			}
			return ""
		}
		if found, err := exec.LookPath(configured); err == nil {
			return found
		}
		return ""
	}
	// A copy shipped next to the running binary outranks PATH.  The installer
	// stages one (see third_party/ffmpeg), it is pinned by a checksum, and the
	// test suite encodes real audio through it -- so it is the build we actually
	// know works.  Preferring a whatever-happens-to-be-installed ffmpeg instead
	// would make the module's behaviour depend on the machine, and a stripped
	// distro build without libopus would fail at the first transcription.
	if exe, err := os.Executable(); err == nil {
		candidate := filepath.Join(filepath.Dir(exe), "ffmpeg.exe")
		if info, err := os.Stat(candidate); err == nil && !info.IsDir() {
			return candidate
		}
	}
	if found, err := exec.LookPath("ffmpeg"); err == nil {
		return found
	}
	return ""
}

func encodeWithFFmpeg(ctx context.Context, ffmpeg string, samples []int16) ([][]byte, error) {
	if ffmpeg == "" {
		return nil, core.Fail("doubaoime", "", core.FailureOther, http.StatusBadGateway,
			errors.New("doubaoime: ffmpeg is not installed or ffmpeg_path is wrong"))
	}
	pcm := make([]byte, len(samples)*2)
	for i, sample := range samples {
		binary.LittleEndian.PutUint16(pcm[i*2:i*2+2], uint16(sample))
	}
	cmd := exec.CommandContext(ctx, ffmpeg,
		"-hide_banner", "-loglevel", "error",
		"-f", "s16le", "-ar", "16000", "-ac", "1", "-i", "pipe:0",
		"-c:a", "libopus", "-b:a", "32k", "-vbr", "on", "-application", "voip",
		"-frame_duration", "20", "-f", "opus", "pipe:1",
	)
	cmd.Stdin = bytes.NewReader(pcm)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = err.Error()
		}
		return nil, core.Fail("doubaoime", "", core.FailureUpstream, http.StatusBadGateway,
			fmt.Errorf("doubaoime: ffmpeg Opus encoding failed: %s", core.Redact(msg)))
	}
	frames, err := parseOggOpus(out)
	if err != nil {
		return nil, err
	}
	if len(frames) == 0 {
		return nil, core.Fail("doubaoime", "", core.FailureUpstream, http.StatusBadGateway,
			errors.New("doubaoime: ffmpeg produced no Opus frames"))
	}
	return frames, nil
}

func parseOggOpus(raw []byte) ([][]byte, error) {
	var (
		frames [][]byte
		packet []byte
	)
	for pos := 0; pos < len(raw); {
		if pos+27 > len(raw) || string(raw[pos:pos+4]) != "OggS" {
			return nil, fmt.Errorf("doubaoime: invalid Ogg Opus output at byte %d", pos)
		}
		segments := int(raw[pos+26])
		headerEnd := pos + 27 + segments
		if headerEnd > len(raw) {
			return nil, fmt.Errorf("doubaoime: truncated Ogg page header")
		}
		lacing := raw[pos+27 : headerEnd]
		body := headerEnd
		for _, size := range lacing {
			end := body + int(size)
			if end > len(raw) {
				return nil, fmt.Errorf("doubaoime: truncated Ogg page body")
			}
			packet = append(packet, raw[body:end]...)
			body = end
			if size < 255 {
				if len(packet) > 0 && !bytes.HasPrefix(packet, []byte("OpusHead")) && !bytes.HasPrefix(packet, []byte("OpusTags")) {
					frames = append(frames, append([]byte(nil), packet...))
				}
				packet = packet[:0]
			}
		}
		pos = body
	}
	return frames, nil
}
