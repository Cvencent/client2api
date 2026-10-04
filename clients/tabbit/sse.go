package tabbit

import (
	"bufio"
	"io"
	"strings"
)

// sseFrame is one decoded server-sent-event frame: the optional event name and
// the joined data payload (multi-line "data:" fields are joined with "\n").
type sseFrame struct {
	Event string
	Data  string
}

// sseReader is a tolerant server-sent-events frame reader.
//
// It deliberately does not assume a frame shape: comments and unknown fields
// are ignored, CRLF and bare LF both work, a last line without a trailing
// newline still counts, and a stream that ends without a blank line still
// yields its final frame.  The upstream is a browser bridge, so its framing is
// not guaranteed to be byte-perfect.
type sseReader struct {
	r *bufio.Reader
}

func newSSEReader(r io.Reader) *sseReader {
	return &sseReader{r: bufio.NewReaderSize(r, 64<<10)}
}

// next returns the next frame, or io.EOF at the end of the stream.
func (s *sseReader) next() (sseFrame, error) {
	var (
		fr   sseFrame
		data []string
	)
	for {
		line, err := s.readLine()
		if err != nil {
			if len(data) > 0 || fr.Event != "" {
				fr.Data = strings.Join(data, "\n")
				return fr, nil
			}
			return sseFrame{}, err
		}
		if line == "" {
			if len(data) == 0 && fr.Event == "" {
				continue // keep-alive blank line
			}
			fr.Data = strings.Join(data, "\n")
			return fr, nil
		}
		if strings.HasPrefix(line, ":") {
			continue // comment
		}
		name, value, found := strings.Cut(line, ":")
		if !found {
			name, value = line, ""
		}
		value = strings.TrimPrefix(value, " ")
		switch name {
		case "data":
			data = append(data, value)
		case "event":
			fr.Event = value
		default:
			// id / retry / unknown fields carry nothing we need.
		}
	}
}

// readLine returns one line without its terminator.  A final line that has no
// newline is returned with a nil error, and the following call reports io.EOF.
func (s *sseReader) readLine() (string, error) {
	line, err := s.r.ReadString('\n')
	if err != nil {
		if line == "" {
			return "", err
		}
		return strings.TrimRight(line, "\r\n"), nil
	}
	return strings.TrimRight(line, "\r\n"), nil
}
