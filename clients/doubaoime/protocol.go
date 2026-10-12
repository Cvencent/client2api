package doubaoime

import "fmt"

type wsMessage struct {
	RequestID     string
	TaskID        string
	ServiceName   string
	MessageType   string
	StatusCode    int
	StatusMessage string
	ResultJSON    string
	Token         string
	MethodName    string
	Payload       string
	AudioData     []byte
	FrameState    int
}

func marshalMessage(msg wsMessage) []byte {
	var out []byte
	out = appendStringField(out, 2, msg.Token)
	out = appendStringField(out, 3, msg.ServiceName)
	out = appendStringField(out, 4, msg.MessageType)
	out = appendStringField(out, 5, msg.MethodName)
	out = appendStringField(out, 6, msg.Payload)
	if msg.StatusCode != 0 {
		out = appendVarintField(out, 5, uint64(msg.StatusCode))
	}
	switch {
	case len(msg.AudioData) > 0:
		out = appendBytesField(out, 7, msg.AudioData)
	default:
		out = appendStringField(out, 7, msg.ResultJSON)
	}
	out = appendStringField(out, 8, msg.RequestID)
	if msg.FrameState != 0 {
		out = appendVarintField(out, 9, uint64(msg.FrameState))
	}
	return out
}

func parseMessage(raw []byte) (wsMessage, error) {
	var msg wsMessage
	for len(raw) > 0 {
		key, n, err := readVarint(raw)
		if err != nil {
			return wsMessage{}, err
		}
		raw = raw[n:]
		field := int(key >> 3)
		wire := int(key & 7)
		switch wire {
		case 0:
			value, n, err := readVarint(raw)
			if err != nil {
				return wsMessage{}, err
			}
			raw = raw[n:]
			switch field {
			case 5:
				msg.StatusCode = int(value)
			case 9:
				msg.FrameState = int(value)
			}
		case 2:
			length, n, err := readVarint(raw)
			if err != nil {
				return wsMessage{}, err
			}
			raw = raw[n:]
			if length > uint64(len(raw)) {
				return wsMessage{}, fmt.Errorf("doubaoime: protobuf field %d length %d exceeds payload %d", field, length, len(raw))
			}
			value := raw[:length]
			raw = raw[length:]
			switch field {
			case 1:
				msg.RequestID = string(value)
			case 2:
				msg.TaskID = string(value)
				msg.Token = string(value)
			case 3:
				msg.ServiceName = string(value)
			case 4:
				msg.MessageType = string(value)
			case 5:
				msg.MethodName = string(value)
			case 6:
				msg.StatusMessage = string(value)
				msg.Payload = string(value)
			case 7:
				msg.ResultJSON = string(value)
				msg.AudioData = append([]byte(nil), value...)
			case 8:
				msg.RequestID = string(value)
			case 9:
				msg.MethodName = string(value)
			case 10:
				msg.Payload = string(value)
			case 11:
				msg.AudioData = append([]byte(nil), value...)
			case 12:
				msg.Token = string(value)
			}
		default:
			return wsMessage{}, fmt.Errorf("doubaoime: unsupported protobuf wire type %d for field %d", wire, field)
		}
	}
	return msg, nil
}

func appendStringField(dst []byte, field int, value string) []byte {
	if value == "" {
		return dst
	}
	return appendBytesField(dst, field, []byte(value))
}

func appendBytesField(dst []byte, field int, value []byte) []byte {
	if len(value) == 0 {
		return dst
	}
	dst = appendVarint(dst, uint64(field<<3|2))
	dst = appendVarint(dst, uint64(len(value)))
	return append(dst, value...)
}

func appendVarintField(dst []byte, field int, value uint64) []byte {
	dst = appendVarint(dst, uint64(field<<3))
	return appendVarint(dst, value)
}

func appendVarint(dst []byte, value uint64) []byte {
	for value >= 0x80 {
		dst = append(dst, byte(value)|0x80)
		value >>= 7
	}
	return append(dst, byte(value))
}

func readVarint(raw []byte) (uint64, int, error) {
	var value uint64
	for i := 0; i < len(raw) && i < 10; i++ {
		b := raw[i]
		if i == 9 && b > 1 {
			return 0, 0, fmt.Errorf("doubaoime: protobuf varint overflow")
		}
		value |= uint64(b&0x7f) << (7 * i)
		if b < 0x80 {
			return value, i + 1, nil
		}
	}
	return 0, 0, fmt.Errorf("doubaoime: truncated protobuf varint")
}
