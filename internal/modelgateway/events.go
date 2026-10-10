package modelgateway

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
)

const maxModelEventBytes = 1 << 20

func responseHasError(data []byte) bool {
	var envelope map[string]json.RawMessage
	if json.Unmarshal(data, &envelope) != nil {
		return true
	}
	if value, ok := envelope["error"]; ok && !bytes.Equal(value, []byte("null")) {
		return true
	}
	var kind string
	return json.Unmarshal(envelope["type"], &kind) == nil && kind == "error"
}

func safeModelError(protocol string) []byte {
	if protocol == "anthropic-messages" {
		return []byte("event: error\ndata: {\"type\":\"error\",\"error\":{\"type\":\"api_error\",\"message\":\"model_upstream_failed\"}}\n\n")
	}
	return []byte("data: {\"error\":{\"type\":\"api_error\",\"code\":\"model_upstream_failed\",\"message\":\"model_upstream_failed\"}}\n\n")
}

// sanitizeEvent examines a complete bounded SSE event, so diagnostic content split across TCP
// reads or multiple data lines cannot bypass error normalization. Successful protocol events
// keep their exact bytes, including tool deltas and provider extensions.
func sanitizeEvent(event []byte, protocol string) ([]byte, error) {
	var data []string
	errorEvent := false
	normalized := strings.TrimPrefix(string(event), "\uFEFF")
	normalized = strings.ReplaceAll(strings.ReplaceAll(normalized, "\r\n", "\n"), "\r", "\n")
	for _, line := range strings.Split(normalized, "\n") {
		if value, ok := strings.CutPrefix(line, "event:"); ok && strings.TrimSpace(value) == "error" {
			errorEvent = true
		}
		if value, ok := strings.CutPrefix(line, "data:"); ok {
			data = append(data, strings.TrimPrefix(value, " "))
		}
	}
	payload := []byte(strings.Join(data, "\n"))
	if len(payload) > 0 && !bytes.Equal(payload, []byte("[DONE]")) {
		errorEvent = errorEvent || !json.Valid(payload) || responseHasError(payload) || !validProtocolEvent(payload, protocol)
	}
	if errorEvent {
		return safeModelError(protocol), errors.New("model upstream failed")
	}
	return event, nil
}

// forwardEvents flushes each bounded protocol event. Write deadlines prevent a non-reading
// downstream from retaining an authorized request or its watcher forever.
func forwardEvents(w http.ResponseWriter, body io.Reader, protocol string, responseIO *responseIO) error {
	reader := bufio.NewReaderSize(body, 32<<10)
	var event []byte
	write := func() error {
		value, sanitizeErr := sanitizeEvent(event, protocol)
		if err := responseIO.prepareWrite(); err != nil {
			return err
		}
		if _, err := w.Write(value); err != nil {
			return err
		}
		if err := http.NewResponseController(w).Flush(); err != nil {
			return err
		}
		event = event[:0]
		return sanitizeErr
	}
	lineLength := 0
	afterCR := false
	for {
		value, err := reader.ReadByte()
		if err != nil {
			if errors.Is(err, io.EOF) {
				if len(event) > 0 {
					return write()
				}
				return nil
			}
			return errors.New("model stream unavailable")
		}
		if len(event)+1 > maxModelEventBytes {
			event = safeModelError(protocol)
			_ = write()
			return errors.New("model event limit exceeded")
		}
		event = append(event, value)
		if value == '\n' && afterCR {
			afterCR = false
			continue
		}
		afterCR = value == '\r'
		if value == '\r' || value == '\n' {
			if lineLength == 0 {
				if writeErr := write(); writeErr != nil {
					return writeErr
				}
			}
			lineLength = 0
		} else {
			lineLength++
		}
	}
}
