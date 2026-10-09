// Package gitproxy implements the building blocks of a policy-governed git
// smart-HTTP reverse proxy in front of the configured code hosting providers.
package gitproxy

import (
	"bufio"
	"bytes"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"strings"
)

// maxCommandSection bounds the pkt-line command section of a git-receive-pack
// request. A command section that exceeds it is rejected fail-closed; it never
// bounds the packfile, which the parser does not touch.
const maxCommandSection = 1 << 20

// Pkt-line lengths of the special (non-data) packets.
const (
	flushPktLen        = 0
	delimPktLen        = 1
	responseEndPktLen  = 2
	pktLineHeaderLen   = 4
	minDataPktLineLen  = 5 // a data pkt-line must carry at least one payload byte
	objectIDHexLenSHA1 = 40
	objectIDHexLenSHA2 = 64
)

// RefUpdate is one ref command from a git-receive-pack request.
type RefUpdate struct {
	// OldSHA is the currently advertised object id (all-zero when creating).
	OldSHA string
	// NewSHA is the requested new object id (all-zero when deleting).
	NewSHA string
	// Ref is the fully qualified ref name, e.g. "refs/heads/ai/fix".
	Ref string
}

// ReadReceivePackCommands reads the pkt-line command section of a
// git-receive-pack request from br, returning the parsed updates and the exact
// bytes consumed (commands plus the terminating flush packet). It stops at the
// flush packet, so br still yields the packfile that follows; the caller
// reassembles the request body as io.MultiReader(bytes.NewReader(consumed), br).
// A malformed or oversized command section is an error (fail-closed).
//
// The command section grammar is one pkt-line per ref update, the first of the
// form "<old-sha> <new-sha> <ref>\0<capabilities>" (the capability list is
// parsed but discarded) and later ones "<old-sha> <new-sha> <ref>"; a trailing
// LF is optional. consumed is a fresh, stable snapshot: the caller may retain
// and replay it independently of br.
func ReadReceivePackCommands(br *bufio.Reader) (updates []RefUpdate, consumed []byte, err error) {
	var buf bytes.Buffer
	for {
		header := make([]byte, pktLineHeaderLen)
		if _, err := io.ReadFull(br, header); err != nil {
			if errors.Is(err, io.EOF) {
				return nil, nil, errors.New("gitproxy: command section ended without a flush packet")
			}
			return nil, nil, fmt.Errorf("gitproxy: truncated pkt-line length prefix: %w", err)
		}
		length, err := parsePktLen(header)
		if err != nil {
			return nil, nil, err
		}
		switch length {
		case flushPktLen:
			buf.Write(header)
			if len(updates) == 0 {
				return nil, nil, errors.New("gitproxy: command section contains no ref update")
			}
			return updates, buf.Bytes(), nil
		case delimPktLen, responseEndPktLen:
			return nil, nil, fmt.Errorf("gitproxy: pkt-line %q is not valid in a command section", header)
		}
		if length < minDataPktLineLen {
			return nil, nil, fmt.Errorf("gitproxy: invalid pkt-line length %q", header)
		}
		if buf.Len()+length > maxCommandSection {
			return nil, nil, fmt.Errorf("gitproxy: command section exceeds %d bytes", maxCommandSection)
		}
		payload := make([]byte, length-pktLineHeaderLen)
		if _, err := io.ReadFull(br, payload); err != nil {
			return nil, nil, fmt.Errorf("gitproxy: truncated pkt-line payload: %w", err)
		}
		buf.Write(header)
		buf.Write(payload)
		update, err := parseCommand(payload)
		if err != nil {
			return nil, nil, err
		}
		updates = append(updates, update)
	}
}

// parsePktLen decodes the 4-hex-ASCII length prefix, which counts itself.
func parsePktLen(header []byte) (int, error) {
	var raw [2]byte
	if _, err := hex.Decode(raw[:], header); err != nil {
		return 0, fmt.Errorf("gitproxy: invalid pkt-line length prefix %q: %w", header, err)
	}
	return int(raw[0])<<8 | int(raw[1]), nil
}

// parseCommand parses one ref-update command payload. Everything after the
// first NUL is the capability list of the first line and is discarded.
func parseCommand(payload []byte) (RefUpdate, error) {
	line, _, _ := bytes.Cut(payload, []byte{0})
	line = bytes.TrimSuffix(line, []byte("\n"))
	oldID, newID, refName, ok := cutThreeFields(line)
	if !ok {
		return RefUpdate{}, fmt.Errorf("gitproxy: malformed ref update %s: want \"<old> <new> <ref>\"", quoteBrief(line))
	}
	if !isObjectID(string(oldID)) {
		return RefUpdate{}, fmt.Errorf("gitproxy: invalid old object id %s", quoteBrief(oldID))
	}
	if !isObjectID(string(newID)) {
		return RefUpdate{}, fmt.Errorf("gitproxy: invalid new object id %s", quoteBrief(newID))
	}
	if hasControl(string(refName)) {
		return RefUpdate{}, fmt.Errorf("gitproxy: invalid ref name %s", quoteBrief(refName))
	}
	return RefUpdate{OldSHA: string(oldID), NewSHA: string(newID), Ref: string(refName)}, nil
}

// cutThreeFields splits line into exactly three non-empty space-separated
// fields. It reports false when the count is wrong or any field is empty.
func cutThreeFields(line []byte) (first, second, third []byte, ok bool) {
	a, rest, ok := bytes.Cut(line, []byte(" "))
	if !ok {
		return nil, nil, nil, false
	}
	b, c, ok := bytes.Cut(rest, []byte(" "))
	if !ok {
		return nil, nil, nil, false
	}
	if len(a) == 0 || len(b) == 0 || len(c) == 0 {
		return nil, nil, nil, false
	}
	// A further space means more than three fields (spaces are illegal in ref
	// names, so a well-formed command never has them).
	if bytes.ContainsAny(c, " ") {
		return nil, nil, nil, false
	}
	return a, b, c, true
}

// hasControl reports whether s is empty or contains an ASCII control character
// (below 0x20 or DEL). NUL cannot reach this check because it terminates the
// capability list, but it is covered for completeness. Bytes below 0x20 never
// appear inside a valid UTF-8 sequence, so this is exact for byte and rune
// alike.
func hasControl(s string) bool {
	if s == "" {
		return true
	}
	for i := 0; i < len(s); i++ {
		if b := s[i]; b < 0x20 || b == 0x7f {
			return true
		}
	}
	return false
}

// quoteBrief formats a possibly attacker-sized payload fragment for an error
// message, bounded so malformed input cannot bloat logs. %q escapes control
// characters, keeping the message single-line.
func quoteBrief(s []byte) string {
	const max = 64
	if len(s) > max {
		return fmt.Sprintf("%q...", s[:max])
	}
	return fmt.Sprintf("%q", s)
}

// isObjectID reports whether s is a SHA-1 (40) or SHA-256 (64) object id in
// lowercase hex, the exact form git emits on the wire. Anything else (including
// uppercase hex) is rejected so the policy check fails closed.
func isObjectID(s string) bool {
	if len(s) != objectIDHexLenSHA1 && len(s) != objectIDHexLenSHA2 {
		return false
	}
	return strings.IndexFunc(s, func(r rune) bool {
		return !((r >= '0' && r <= '9') || (r >= 'a' && r <= 'f'))
	}) < 0
}
