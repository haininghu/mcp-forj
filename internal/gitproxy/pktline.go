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

// maxPushOptionsSection bounds the pkt-line push-options section of a
// git-receive-pack request the same way maxCommandSection bounds the command
// section: an oversized section is rejected fail-closed and never reached the
// provider.
const maxPushOptionsSection = 1 << 20

// maxPushOption bounds one push option. Options are short "key[=value]" strings,
// so a longer payload is malformed or hostile input and fails closed. It also
// keeps option-sized data out of the policy decision and the logs.
const maxPushOption = 4096

// pushOptionsCapability is the capability a client announces on the first
// command line to say that a push-options section follows the command flush.
const pushOptionsCapability = "push-options"

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

// ReceivePack is the parsed preamble of a git-receive-pack request.
type ReceivePack struct {
	// Updates are the ref commands of the command section.
	Updates []RefUpdate
	// PushOptions are the pkt-line push options (empty when none were sent).
	PushOptions []string
	// Consumed is every byte read (command section and, when present, the
	// push-options section, each including its flush), so callers reassemble
	// the body as io.MultiReader(bytes.NewReader(Consumed), br).
	Consumed []byte
}

// ReadReceivePack reads the command section of a git-receive-pack request and,
// when the client negotiated the "push-options" capability, the following
// push-options section. It stops at the packfile; a malformed or oversized
// section is an error (fail-closed).
//
// The command section grammar is one pkt-line per ref update, the first of the
// form "<old-sha> <new-sha> <ref>\0<capabilities>" and later ones
// "<old-sha> <new-sha> <ref>"; a trailing LF is optional. The capability list
// after the NUL is parsed to detect the push-options negotiation. Each
// push-options payload is one option, sent with an optional trailing LF that is
// stripped, mirroring what git's receive-pack does. Consumed is a fresh, stable
// snapshot of both sections including their flush packets: the caller may retain
// and replay it independently of br, and the packfile is never touched.
func ReadReceivePack(br *bufio.Reader) (*ReceivePack, error) {
	var consumed bytes.Buffer
	var updates []RefUpdate
	pushOptions := false
	commandBytes := 0
	for {
		payload, flush, err := nextPktLine(br, &consumed, commandBytes, maxCommandSection, "command")
		if err != nil {
			return nil, err
		}
		if flush {
			if len(updates) == 0 {
				return nil, errors.New("gitproxy: command section contains no ref update")
			}
			rp := &ReceivePack{Updates: updates}
			if pushOptions {
				// Only a client that negotiated the capability may send the
				// section; reading it whenever the capability is present is the
				// fail-closed choice. Without it the packfile follows the flush
				// directly and stays untouched in br.
				rp.PushOptions, err = readPushOptions(br, &consumed)
				if err != nil {
					return nil, err
				}
			}
			// Snapshot last: Consumed then covers every section read so far, each
			// including its flush packet, while br holds exactly the packfile.
			rp.Consumed = consumed.Bytes()
			return rp, nil
		}
		update, err := parseCommand(payload)
		if err != nil {
			return nil, err
		}
		if len(updates) == 0 {
			pushOptions = negotiatesPushOptions(payload)
		}
		commandBytes += len(payload) + pktLineHeaderLen
		updates = append(updates, update)
	}
}

// readPushOptions reads the push-options section that follows the command flush
// and appends every byte to consumed, so the section replays verbatim. Each
// payload is one option; the section ends at the next flush packet.
func readPushOptions(br *bufio.Reader, consumed *bytes.Buffer) ([]string, error) {
	var options []string
	optionBytes := 0
	for {
		payload, flush, err := nextPktLine(br, consumed, optionBytes, maxPushOptionsSection, "push-options")
		if err != nil {
			return nil, err
		}
		if flush {
			return options, nil
		}
		option, err := parsePushOption(payload)
		if err != nil {
			return nil, err
		}
		optionBytes += len(payload) + pktLineHeaderLen
		options = append(options, option)
	}
}

// nextPktLine reads one pkt-line from br, appends its exact bytes (length prefix
// plus payload, or just the flush packet) to consumed, and returns the payload.
// sectionBytes is what the section already holds and limit bounds it; a
// malformed packet, a special packet that never appears in a request section or
// an oversized section is an error, so the caller fails closed. section names
// the section in the error messages.
func nextPktLine(
	br *bufio.Reader, consumed *bytes.Buffer, sectionBytes, limit int, section string,
) (payload []byte, flush bool, err error) {
	header := make([]byte, pktLineHeaderLen)
	if _, err := io.ReadFull(br, header); err != nil {
		if errors.Is(err, io.EOF) {
			return nil, false, fmt.Errorf("gitproxy: %s section ended without a flush packet", section)
		}
		return nil, false, fmt.Errorf("gitproxy: truncated pkt-line length prefix: %w", err)
	}
	length, err := parsePktLen(header)
	if err != nil {
		return nil, false, err
	}
	switch length {
	case flushPktLen:
		consumed.Write(header)
		return nil, true, nil
	case delimPktLen, responseEndPktLen:
		return nil, false, fmt.Errorf("gitproxy: pkt-line %q is not valid in a %s section", header, section)
	}
	if length < minDataPktLineLen {
		return nil, false, fmt.Errorf("gitproxy: invalid pkt-line length %q", header)
	}
	if sectionBytes+length > limit {
		return nil, false, fmt.Errorf("gitproxy: %s section exceeds %d bytes", section, limit)
	}
	payload = make([]byte, length-pktLineHeaderLen)
	if _, err := io.ReadFull(br, payload); err != nil {
		return nil, false, fmt.Errorf("gitproxy: truncated pkt-line payload: %w", err)
	}
	consumed.Write(header)
	consumed.Write(payload)
	return payload, false, nil
}

// negotiatesPushOptions reports whether the first command payload carries the
// "push-options" capability after its NUL. The list is space-separated and a
// capability may carry an "=value" suffix, so the token name decides: reading
// the section whenever the name appears is the fail-closed choice, because a
// provider that honored such a token would otherwise receive unchecked options.
func negotiatesPushOptions(payload []byte) bool {
	_, capabilities, ok := bytes.Cut(payload, []byte{0})
	if !ok {
		return false
	}
	for _, capability := range strings.Fields(string(capabilities)) {
		if name, _, _ := strings.Cut(capability, "="); name == pushOptionsCapability {
			return true
		}
	}
	return false
}

// parsePushOption validates one push-options payload: git terminates each option
// with an optional LF that receive-pack strips, the option itself must not
// contain a NUL, must not be empty and must stay within maxPushOption.
func parsePushOption(payload []byte) (string, error) {
	option := bytes.TrimSuffix(payload, []byte("\n"))
	if bytes.IndexByte(option, 0) >= 0 {
		return "", fmt.Errorf("gitproxy: push option %s contains a NUL", quoteBrief(option))
	}
	if len(option) == 0 {
		return "", errors.New("gitproxy: empty push option")
	}
	if len(option) > maxPushOption {
		return "", fmt.Errorf("gitproxy: push option exceeds %d bytes", maxPushOption)
	}
	return string(option), nil
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
