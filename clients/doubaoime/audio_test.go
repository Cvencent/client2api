package doubaoime

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"math"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"client2api/internal/core"
)

func TestDecodeWAVMono16k(t *testing.T) {
	want := []int16{-1000, 0, 1000, 2000}
	raw := makeTestWAV(16000, 1, 16, want)
	got, err := decodeWAV(raw)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.samples) != len(want) {
		t.Fatalf("samples = %d, want %d", len(got.samples), len(want))
	}
	for i := range want {
		if got.samples[i] != want[i] {
			t.Fatalf("sample %d = %d, want %d", i, got.samples[i], want[i])
		}
	}
}

func TestConvertToPCM16kResamplesStereo(t *testing.T) {
	in := make([]int16, 0, 320)
	for i := 0; i < 160; i++ {
		in = append(in, int16(i*10), int16(i*10+5))
	}
	out := convertToPCM16k(in, 8000, 2)
	if len(out) != 320 {
		t.Fatalf("resampled length = %d, want 320", len(out))
	}
}

func TestDecodeWAVRejectsInvalidHeader(t *testing.T) {
	_, err := decodeWAV([]byte("not a wave"))
	if !errors.Is(err, core.ErrInvalidAudio) {
		t.Fatalf("error = %v", err)
	}
}

func TestIsSilent(t *testing.T) {
	if !isSilent(make([]int16, 320)) {
		t.Fatal("zero samples should be silent")
	}
	voice := make([]int16, 320)
	for i := range voice {
		voice[i] = int16(300 * math.Sin(float64(i)/8))
	}
	if isSilent(voice) {
		t.Fatal("waveform should not be silent")
	}
}

func TestParseOggOpusPackets(t *testing.T) {
	packets := [][]byte{
		[]byte("OpusHead"),
		[]byte("OpusTags"),
		{1, 2, 3},
		{4, 5},
	}
	raw := makeTestOggPage(7, packets)
	got, err := parseOggOpus(raw)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || !bytes.Equal(got[0], []byte{1, 2, 3}) || !bytes.Equal(got[1], []byte{4, 5}) {
		t.Fatalf("packets = %v", got)
	}
}

func TestFFmpegEncodesTwentyMillisecondFrames(t *testing.T) {
	ffmpeg := findFFmpeg("")
	if ffmpeg == "" {
		t.Skip("ffmpeg is not installed")
	}
	samples := make([]int16, 1600)
	for i := range samples {
		samples[i] = int16(600 * math.Sin(float64(i)/10))
	}
	frames, err := encodeWithFFmpeg(t.Context(), ffmpeg, samples)
	if err != nil {
		t.Fatal(err)
	}
	if len(frames) < 4 {
		t.Fatalf("encoded %d frames, want at least 4", len(frames))
	}
}

func makeTestWAV(sampleRate, channels, bits int, samples []int16) []byte {
	dataSize := len(samples) * 2
	var out bytes.Buffer
	out.WriteString("RIFF")
	_ = binary.Write(&out, binary.LittleEndian, uint32(36+dataSize))
	out.WriteString("WAVE")
	out.WriteString("fmt ")
	_ = binary.Write(&out, binary.LittleEndian, uint32(16))
	_ = binary.Write(&out, binary.LittleEndian, uint16(1))
	_ = binary.Write(&out, binary.LittleEndian, uint16(channels))
	_ = binary.Write(&out, binary.LittleEndian, uint32(sampleRate))
	_ = binary.Write(&out, binary.LittleEndian, uint32(sampleRate*channels*bits/8))
	_ = binary.Write(&out, binary.LittleEndian, uint16(channels*bits/8))
	_ = binary.Write(&out, binary.LittleEndian, uint16(bits))
	out.WriteString("data")
	_ = binary.Write(&out, binary.LittleEndian, uint32(dataSize))
	for _, sample := range samples {
		_ = binary.Write(&out, binary.LittleEndian, sample)
	}
	return out.Bytes()
}

func makeTestOggPage(serial uint32, packets [][]byte) []byte {
	var lacing []byte
	var body bytes.Buffer
	for _, packet := range packets {
		left := len(packet)
		for left >= 255 {
			lacing = append(lacing, 255)
			left -= 255
		}
		lacing = append(lacing, byte(left))
		body.Write(packet)
	}
	var out bytes.Buffer
	out.WriteString("OggS")
	out.Write([]byte{0, 0})
	out.Write(make([]byte, 8))
	_ = binary.Write(&out, binary.LittleEndian, serial)
	_ = binary.Write(&out, binary.LittleEndian, uint32(1))
	_ = binary.Write(&out, binary.LittleEndian, uint32(0))
	out.WriteByte(byte(len(lacing)))
	out.Write(lacing)
	out.Write(body.Bytes())
	return out.Bytes()
}

// TestVendoredFFmpegProducesProtocolFrames exercises the ffmpeg.exe that ships
// in the installer, not whatever happens to be on the machine.
//
// The existing TestFFmpegEncodesTwentyMillisecondFrames calls findFFmpeg(""),
// which prefers PATH -- so on a developer box or a CI runner with ffmpeg
// installed it silently tests someone else's build, and where none is
// installed it skips entirely. That left the vendored binary untested: a
// rebuild that dropped the opus muxer or the pcm_s16le decoder (both are easy
// to lose to --disable-everything) would still pass CI and then fail on the
// operator's first transcription.
//
// This pins the three properties the module depends on at runtime:
//   - the exact command line in encodeWithFFmpeg is accepted,
//   - the output is Ogg the module's parser can walk, and
//   - it yields roughly one frame per 20 ms of audio.
func TestVendoredFFmpegProducesProtocolFrames(t *testing.T) {
	if runtime.GOOS != "windows" {
		// third_party/ffmpeg ships a Windows .exe, which cannot be exec'd on the
		// Linux and macOS runners. The checksum test below still guards it there.
		t.Skip("the vendored ffmpeg is a Windows binary; encode coverage runs on Windows")
	}
	vendored := filepath.Join("..", "..", "third_party", "ffmpeg", "ffmpeg.exe")
	if _, err := os.Stat(vendored); err != nil {
		t.Skipf("vendored ffmpeg is not present: %v", err)
	}

	samples := make([]int16, 16000) // one second
	for i := range samples {
		samples[i] = int16(6000 * math.Sin(float64(i)/8))
	}
	frames, err := encodeWithFFmpeg(t.Context(), vendored, samples)
	if err != nil {
		t.Fatalf("the bundled ffmpeg rejected the module's command line: %v", err)
	}
	// One second of 20 ms frames is ~50 packets. The Ogg Opus header packets are
	// skipped by the parser, so allow slack in both directions but insist the
	// output is a real frame stream rather than a header or an error.
	if len(frames) < 40 || len(frames) > 60 {
		t.Fatalf("encoded %d frames for 1s of 20ms audio, want ~50", len(frames))
	}
	for i, f := range frames {
		if len(f) == 0 {
			t.Fatalf("frame %d is empty", i)
		}
	}
}

// TestVendoredFFmpegChecksumMatches pins the committed binary against the
// recorded digest. build.ps1 refuses to package a binary that fails this, and
// so should anyone who swaps it by hand.
func TestVendoredFFmpegChecksumMatches(t *testing.T) {
	dir := filepath.Join("..", "..", "third_party", "ffmpeg")
	raw, err := os.ReadFile(filepath.Join(dir, "ffmpeg.sha256"))
	if err != nil {
		t.Skipf("no recorded checksum: %v", err)
	}
	fields := strings.Fields(string(raw))
	if len(fields) == 0 {
		t.Fatal("ffmpeg.sha256 is empty")
	}
	raw2, err := os.ReadFile(filepath.Join(dir, "ffmpeg.exe"))
	if err != nil {
		t.Skipf("vendored ffmpeg is not present: %v", err)
	}
	got := sha256.Sum256(raw2)
	if hex.EncodeToString(got[:]) != strings.ToLower(fields[0]) {
		t.Fatalf("ffmpeg.exe sha256 = %s, want %s (see third_party/ffmpeg/README.md)", hex.EncodeToString(got[:]), fields[0])
	}
}
