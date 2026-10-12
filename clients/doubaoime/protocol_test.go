package doubaoime

import (
	"bytes"
	"testing"
)

func TestMarshalRequestFields(t *testing.T) {
	raw := marshalMessage(wsMessage{
		Token:       "key-1",
		ServiceName: "ASR",
		MethodName:  "StartSession",
		Payload:     `{"audio_info":{"channel":1}}`,
		AudioData:   []byte{1, 2, 3},
		RequestID:   "req-1",
		FrameState:  3,
	})
	got, err := parseMessage(raw)
	if err != nil {
		t.Fatal(err)
	}
	if got.Token != "key-1" || got.ServiceName != "ASR" || got.MethodName != "StartSession" {
		t.Fatalf("message = %+v", got)
	}
	if got.Payload != `{"audio_info":{"channel":1}}` || got.RequestID != "req-1" || got.FrameState != 3 {
		t.Fatalf("message = %+v", got)
	}
	if !bytes.Equal(got.AudioData, []byte{1, 2, 3}) {
		t.Fatalf("audio = %v", got.AudioData)
	}
}

func TestParseResponseSkipsUnknownFields(t *testing.T) {
	raw := testAppendProtoBytes(nil, 99, []byte("ignored"))
	raw = testAppendProtoString(raw, 1, "req-2")
	raw = testAppendProtoString(raw, 4, "SessionFinished")
	raw = testAppendProtoVarint(raw, 5, 0)
	raw = testAppendProtoString(raw, 7, `{"results":[{"text":"你好","is_interim":false}]}`)

	got, err := parseMessage(raw)
	if err != nil {
		t.Fatal(err)
	}
	if got.RequestID != "req-2" || got.MessageType != "SessionFinished" || got.StatusCode != 0 {
		t.Fatalf("response = %+v", got)
	}
	if got.ResultJSON != `{"results":[{"text":"你好","is_interim":false}]}` {
		t.Fatalf("result = %q", got.ResultJSON)
	}
}

func TestParseMessageRejectsTruncatedField(t *testing.T) {
	if _, err := parseMessage([]byte{0x12, 0x05, 'a'}); err == nil {
		t.Fatal("truncated field accepted")
	}
}

func testAppendProtoVarint(dst []byte, field int, value int) []byte {
	dst = testAppendVarint(dst, uint64(field<<3))
	return testAppendVarint(dst, uint64(value))
}

func testAppendProtoString(dst []byte, field int, value string) []byte {
	return testAppendProtoBytes(dst, field, []byte(value))
}

func testAppendProtoBytes(dst []byte, field int, value []byte) []byte {
	dst = testAppendVarint(dst, uint64(field<<3|2))
	dst = testAppendVarint(dst, uint64(len(value)))
	return append(dst, value...)
}

func testAppendVarint(dst []byte, value uint64) []byte {
	for value >= 0x80 {
		dst = append(dst, byte(value)|0x80)
		value >>= 7
	}
	return append(dst, byte(value))
}
