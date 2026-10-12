package doubaoime

import (
	"bytes"
	"encoding/binary"
	"errors"
	"math"
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
