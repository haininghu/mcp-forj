package gitproxy

import (
	"bufio"
	"bytes"
	"fmt"
	"io"
	"reflect"
	"strings"
	"testing"
)

var (
	sha1Old  = strings.Repeat("a", 40)
	sha1New  = strings.Repeat("b", 40)
	sha1Zero = strings.Repeat("0", 40)
	sha2Old  = strings.Repeat("c", 64)
	sha2New  = strings.Repeat("d", 64)
)

// pkt encodes s as one pkt-line: 4-hex length (including the prefix) plus body.
func pkt(s string) []byte {
	return append([]byte(fmt.Sprintf("%04x", len(s)+4)), s...)
}

// pkts joins command payloads with their pkt-line framing and a trailing flush.
func pkts(lines ...string) []byte {
	var out []byte
	for _, l := range lines {
		out = append(out, pkt(l)...)
	}
	return append(out, []byte("0000")...)
}

func TestReadReceivePackCommands(t *testing.T) {
	packfile := []byte("PACK-BODY-BYTES")
	tests := []struct {
		name  string
		input []byte
		want  []RefUpdate
		tail  []byte
	}{
		{
			name: "single update with capabilities",
			input: append(
				pkts(sha1Old+" "+sha1New+" refs/heads/ai/fix\x00report-status delete-refs side-band-64k\n"),
				packfile...),
			want: []RefUpdate{{OldSHA: sha1Old, NewSHA: sha1New, Ref: "refs/heads/ai/fix"}},
			tail: packfile,
		},
		{
			name: "multiple updates",
			input: append(
				pkts(
					sha1Old+" "+sha1New+" refs/heads/main\x00report-status\n",
					sha1New+" "+sha1Zero+" refs/heads/old",
					sha1Zero+" "+sha1Old+" refs/tags/v1",
				), packfile...),
			want: []RefUpdate{
				{OldSHA: sha1Old, NewSHA: sha1New, Ref: "refs/heads/main"},
				{OldSHA: sha1New, NewSHA: sha1Zero, Ref: "refs/heads/old"},
				{OldSHA: sha1Zero, NewSHA: sha1Old, Ref: "refs/tags/v1"},
			},
			tail: packfile,
		},
		{
			name: "create has all-zero old",
			input: append(
				pkts(sha1Zero+" "+sha1New+" refs/heads/ai/new-branch"),
				packfile...),
			want: []RefUpdate{{OldSHA: sha1Zero, NewSHA: sha1New, Ref: "refs/heads/ai/new-branch"}},
			tail: packfile,
		},
		{
			name: "delete has all-zero new",
			input: append(
				pkts(sha1Old+" "+sha1Zero+" refs/heads/gone"),
				packfile...),
			want: []RefUpdate{{OldSHA: sha1Old, NewSHA: sha1Zero, Ref: "refs/heads/gone"}},
			tail: packfile,
		},
		{
			name: "64-hex object ids are accepted",
			input: append(
				pkts(sha2Old+" "+sha2New+" refs/heads/ai/fix\x00object-format=sha256\n"),
				packfile...),
			want: []RefUpdate{{OldSHA: sha2Old, NewSHA: sha2New, Ref: "refs/heads/ai/fix"}},
			tail: packfile,
		},
		{
			name:  "flush only followed by packfile is rejected",
			input: append([]byte("0000"), packfile...),
			want:  nil,
			tail:  nil,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.want == nil {
				// flush-only without any command must fail closed.
				br := bufio.NewReader(bytes.NewReader(tt.input))
				if _, _, err := ReadReceivePackCommands(br); err == nil {
					t.Fatal("ReadReceivePackCommands succeeded, want error")
				}
				return
			}
			br := bufio.NewReader(bytes.NewReader(tt.input))
			updates, consumed, err := ReadReceivePackCommands(br)
			if err != nil {
				t.Fatalf("ReadReceivePackCommands: %v", err)
			}
			if !reflect.DeepEqual(updates, tt.want) {
				t.Errorf("updates = %+v, want %+v", updates, tt.want)
			}
			wantConsumed := tt.input[:len(tt.input)-len(tt.tail)]
			if !bytes.Equal(consumed, wantConsumed) {
				t.Errorf("consumed = %q, want the exact command bytes %q", consumed, wantConsumed)
			}
			rest, err := io.ReadAll(br)
			if err != nil {
				t.Fatalf("read remainder: %v", err)
			}
			if !bytes.Equal(rest, tt.tail) {
				t.Errorf("remainder = %q, want %q", rest, tt.tail)
			}
		})
	}
}

func TestReadReceivePackCommandsBodyReassembly(t *testing.T) {
	packfile := []byte{0x50, 0x41, 0x43, 0x4b, 0x00, 0x01, 0xff, 0x80}
	input := append(
		pkts(
			sha1Old+" "+sha1New+" refs/heads/main\x00report-status\n",
			sha1New+" "+sha1Zero+" refs/heads/topic",
		),
		packfile...)

	br := bufio.NewReader(bytes.NewReader(input))
	_, consumed, err := ReadReceivePackCommands(br)
	if err != nil {
		t.Fatalf("ReadReceivePackCommands: %v", err)
	}
	got, err := io.ReadAll(io.MultiReader(bytes.NewReader(consumed), br))
	if err != nil {
		t.Fatalf("reassemble: %v", err)
	}
	if !bytes.Equal(got, input) {
		t.Errorf("reassembled body differs from the original input")
	}
}

func TestReadReceivePackCommandsErrors(t *testing.T) {
	flush := []byte("0000")
	tests := []struct {
		name  string
		input []byte
	}{
		{"missing flush packet", pkt(sha1Old + " " + sha1New + " refs/heads/main")},
		{"empty input", nil},
		{"non-hex length prefix", append([]byte("zzzz"), pkt("junk")...)},
		{"length prefix truncated", []byte("00")},
		{"length 0003 below header size", append([]byte("0003"), flush...)},
		{"empty data packet 0004", append([]byte("0004"), flush...)},
		{"delim packet in commands", append([]byte("0001"), flush...)},
		{"response-end packet in commands", append([]byte("0002"), flush...)},
		{"payload shorter than length", append([]byte("0014"), []byte("abc")...)},
		{"payload truncated mid-line", pkt(sha1Old + " " + sha1New + " refs/heads/main")[:20]},
		{"line without three fields", append(append(pkt("garbage"), flush...), flush...)[:0+len(pkt("garbage"))+len(flush)]},
		{"two fields only", append(append(pkt(sha1Old+" "+sha1New), flush...), flush...)[:len(pkt(sha1Old+" "+sha1New))+len(flush)]},
		{"four fields", append(pkt(sha1Old+" "+sha1New+" refs/heads/x extra"), flush...)},
		{"double space leaves an empty field", append(pkt(sha1Old+"  "+sha1New+" refs/heads/x"), flush...)},
		{"empty ref name", append(pkt(sha1Old+" "+sha1New+" "), flush...)},
		{"old sha too short", append(pkt(strings.Repeat("a", 39)+" "+sha1New+" refs/heads/x"), flush...)},
		{"old sha not hex", append(pkt(strings.Repeat("g", 40)+" "+sha1New+" refs/heads/x"), flush...)},
		{"new sha uppercase hex", append(pkt(sha1Old+" "+strings.Repeat("B", 40)+" refs/heads/x"), flush...)},
		{"NUL before the fields are complete", append(pkt(sha1Old+"\x00"+sha1New+" refs/heads/x"), flush...)},
		{"ref contains a tab", append(pkt(sha1Old+" "+sha1New+" refs/heads/a\tb"), flush...)},
		{"ref contains DEL", append(pkt(sha1Old+" "+sha1New+" refs/heads/a\x7fb"), flush...)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			br := bufio.NewReader(bytes.NewReader(tt.input))
			updates, consumed, err := ReadReceivePackCommands(br)
			if err == nil {
				t.Fatalf("ReadReceivePackCommands = %+v, want an error", updates)
			}
			if updates != nil || consumed != nil {
				t.Errorf("partial results on error: updates = %+v, consumed = %q", updates, consumed)
			}
		})
	}
}

func TestReadReceivePackCommandsRejectsOversizedSection(t *testing.T) {
	// 17 commands of ~64 KiB each exceed the 1 MiB command-section limit.
	bigLine := sha1Old + " " + sha1New + " refs/heads/" + strings.Repeat("x", 65000) + "\n"
	var input []byte
	for range 17 {
		input = append(input, pkt(bigLine)...)
	}
	input = append(input, []byte("0000")...)
	if int64(len(input)) <= maxCommandSection {
		t.Fatalf("test input is %d bytes, want more than %d", len(input), maxCommandSection)
	}

	br := bufio.NewReader(bytes.NewReader(input))
	if _, _, err := ReadReceivePackCommands(br); err == nil {
		t.Fatal("ReadReceivePackCommands accepted an oversized command section, want error")
	}
}
