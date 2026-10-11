package doubao

import (
	"bufio"
	"io"
	"strings"
)

// sse.go reads the vendor's event stream.
//
// The upstream is not spec-perfect:
//
//   - `data:` may arrive with no space after the colon.
//   - frames may be separated by CRLF or a bare LF.
//   - the last frame may be missing its terminating blank line.
//   - comment lines (`: keep-alive`) and unknown fields are skipped.

// sseFrame is one event-stream frame.
type sseFrame struct {
	Event string
	Data  string
}

type sseReader struct {
	r *bufio.Reader
}

func newSSEReader(r io.Reader) *sseReader {
	return &sseReader{r: bufio.NewReaderSize(r, 64<<10)}
}

// next returns the next frame.  io.EOF is returned only when the stream is
// exhausted; a final frame without a trailing blank line is still returned.
func (s *sseReader) next() (sseFrame, error) {
	frame := sseFrame{}
	var data []string
	saw := false
	for {
		line, err := s.readLine()
		if err != nil {
			if saw {
				frame.Data = strings.Join(data, "\n")
				return frame, nil
			}
			return sseFrame{}, err
		}
		if line == "" {
			if !saw {
				continue
			}
			frame.Data = strings.Join(data, "\n")
			return frame, nil
		}
		if strings.HasPrefix(line, ":") {
			continue
		}
		field, value, found := strings.Cut(line, ":")
		if !found {
			continue
		}
		value = strings.TrimPrefix(value, " ")
		switch field {
		case "data":
			data = append(data, value)
			saw = true
		case "event":
			frame.Event = value
			saw = true
		default:
			// id, retry, and whatever else this upstream invents.
		}
	}
}

// readLine returns one line without its terminator.  A last line with no
// newline is returned with a nil error, not as EOF.
func (s *sseReader) readLine() (string, error) {
	line, err := s.r.ReadString('\n')
	if err != nil {
		if len(line) > 0 {
			return strings.TrimRight(line, "\r\n"), nil
		}
		return "", err
	}
	return strings.TrimRight(line, "\r\n"), nil
}
