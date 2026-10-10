package gitproxy

import (
	"bufio"
	"bytes"
	"fmt"
	"io"
	"reflect"
	"slices"
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

// optionPkts frames a push-options section: one pkt-line per option (git
// terminates each with a LF) plus the closing flush packet.
func optionPkts(options ...string) []byte {
	var out []byte
	for _, o := range options {
		out = append(out, pkt(o+"\n")...)
	}
	return append(out, []byte("0000")...)
}

// commandWithCaps is the first command line carrying the given capabilities.
func commandWithCaps(old, new, ref string, caps ...string) string {
	return old + " " + new + " " + ref + "\x00" + strings.Join(caps, " ") + "\n"
}

func TestReadReceivePack(t *testing.T) {
	packfile := []byte("PACK-BODY-BYTES")
	tests := []struct {
		name        string
		input       []byte
		want        []RefUpdate
		wantOptions []string
		tail        []byte
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
			name: "negotiated push options are parsed",
			input: append(
				append(
					pkts(commandWithCaps(sha1Zero, sha1New, "refs/heads/ai/fix", "report-status", "push-options")),
					optionPkts("merge_request.create", "merge_request.target=main")...),
				packfile...),
			want:        []RefUpdate{{OldSHA: sha1Zero, NewSHA: sha1New, Ref: "refs/heads/ai/fix"}},
			wantOptions: []string{"merge_request.create", "merge_request.target=main"},
			tail:        packfile,
		},
		{
			name: "options value may contain spaces and further separators",
			input: append(
				append(
					pkts(commandWithCaps(sha1Zero, sha1New, "refs/heads/ai/fix", "push-options")),
					optionPkts("merge_request.title=fix: a = b", "merge_request.description=multi word")...),
				packfile...),
			want:        []RefUpdate{{OldSHA: sha1Zero, NewSHA: sha1New, Ref: "refs/heads/ai/fix"}},
			wantOptions: []string{"merge_request.title=fix: a = b", "merge_request.description=multi word"},
			tail:        packfile,
		},
		{
			name: "a valued push-options token still negotiates",
			input: append(
				append(
					pkts(commandWithCaps(sha1Zero, sha1New, "refs/heads/ai/fix", "push-options=1")),
					optionPkts("merge_request.create")...),
				packfile...),
			want:        []RefUpdate{{OldSHA: sha1Zero, NewSHA: sha1New, Ref: "refs/heads/ai/fix"}},
			wantOptions: []string{"merge_request.create"},
			tail:        packfile,
		},
		{
			name: "negotiated but empty options section",
			input: append(
				append(
					pkts(commandWithCaps(sha1Zero, sha1New, "refs/heads/ai/fix", "push-options")),
					optionPkts()...),
				packfile...),
			want:        []RefUpdate{{OldSHA: sha1Zero, NewSHA: sha1New, Ref: "refs/heads/ai/fix"}},
			wantOptions: nil,
			tail:        packfile,
		},
		{
			name: "without the capability the bytes belong to the packfile",
			// No push-options capability: the parser must not consume anything
			// after the command flush, even though a pkt-line follows anyway.
			// The upstream receives the stray bytes and rejects them itself.
			input: append(
				append(
					pkts(commandWithCaps(sha1Zero, sha1New, "refs/heads/ai/fix", "report-status")),
					optionPkts("merge_request.create")...),
				packfile...),
			want:        []RefUpdate{{OldSHA: sha1Zero, NewSHA: sha1New, Ref: "refs/heads/ai/fix"}},
			wantOptions: nil,
			tail:        append(optionPkts("merge_request.create"), packfile...),
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
			br := bufio.NewReader(bytes.NewReader(tt.input))
			rp, err := ReadReceivePack(br)
			if tt.want == nil {
				if err == nil {
					t.Fatalf("ReadReceivePack = %+v, want an error", rp)
				}
				return
			}
			if err != nil {
				t.Fatalf("ReadReceivePack: %v", err)
			}
			if !reflect.DeepEqual(rp.Updates, tt.want) {
				t.Errorf("updates = %+v, want %+v", rp.Updates, tt.want)
			}
			if !slices.Equal(rp.PushOptions, tt.wantOptions) {
				t.Errorf("push options = %q, want %q", rp.PushOptions, tt.wantOptions)
			}
			wantConsumed := tt.input[:len(tt.input)-len(tt.tail)]
			if !bytes.Equal(rp.Consumed, wantConsumed) {
				t.Errorf("consumed = %q, want the exact section bytes %q", rp.Consumed, wantConsumed)
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

func TestReadReceivePackBodyReassembly(t *testing.T) {
	packfile := []byte{0x50, 0x41, 0x43, 0x4b, 0x00, 0x01, 0xff, 0x80}
	input := append(
		append(
			pkts(
				commandWithCaps(sha1Old, sha1New, "refs/heads/main", "report-status", "push-options"),
				sha1New+" "+sha1Zero+" refs/heads/topic",
			),
			optionPkts("merge_request.create", "merge_request.title=a title with spaces")...),
		packfile...)

	br := bufio.NewReader(bytes.NewReader(input))
	rp, err := ReadReceivePack(br)
	if err != nil {
		t.Fatalf("ReadReceivePack: %v", err)
	}
	wantOptionStrings := []string{"merge_request.create", "merge_request.title=a title with spaces"}
	if !slices.Equal(rp.PushOptions, wantOptionStrings) {
		t.Errorf("push options = %q, want %q", rp.PushOptions, wantOptionStrings)
	}
	got, err := io.ReadAll(io.MultiReader(bytes.NewReader(rp.Consumed), br))
	if err != nil {
		t.Fatalf("reassemble: %v", err)
	}
	if !bytes.Equal(got, input) {
		t.Errorf("reassembled body differs from the original input:\n got %q\nwant %q", got, input)
	}
}

func TestReadReceivePackErrors(t *testing.T) {
	flush := []byte("0000")
	negotiate := commandWithCaps(sha1Zero, sha1New, "refs/heads/ai/fix", "push-options")
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
		{"options section missing after negotiation", pkts(negotiate)},
		{"options section truncated mid-packet", append(pkts(negotiate), []byte("0010merge")...)},
		{"options section without flush", append(pkts(negotiate), pkt("merge_request.create")...)},
		{"options section with a delim packet", append(append(pkts(negotiate), []byte("0001")...), flush...)},
		{"empty push option payload", append(append(pkts(negotiate), []byte("0004")...), flush...)},
		{"push option is only a newline", append(append(pkts(negotiate), pkt("\n")...), flush...)},
		{"push option contains a NUL", append(append(pkts(negotiate), pkt("merge_request.title=a\x00b")...), flush...)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			br := bufio.NewReader(bytes.NewReader(tt.input))
			rp, err := ReadReceivePack(br)
			if err == nil {
				t.Fatalf("ReadReceivePack = %+v, want an error", rp)
			}
			if rp != nil {
				t.Errorf("partial result on error: %+v", rp)
			}
		})
	}
}

func TestReadReceivePackRejectsOversizedCommandSection(t *testing.T) {
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
	if _, err := ReadReceivePack(br); err == nil {
		t.Fatal("ReadReceivePack accepted an oversized command section, want error")
	}
}

func TestReadReceivePackRejectsOversizedOptions(t *testing.T) {
	negotiate := commandWithCaps(sha1Zero, sha1New, "refs/heads/ai/fix", "push-options")
	t.Run("single option exceeds maxPushOption", func(t *testing.T) {
		big := "merge_request.description=" + strings.Repeat("x", maxPushOption)
		input := append(append(pkts(negotiate), optionPkts(big)...), []byte("PACK")...)
		if _, err := ReadReceivePack(bufio.NewReader(bytes.NewReader(input))); err == nil {
			t.Fatal("accepted an option longer than maxPushOption, want error")
		}
	})
	t.Run("options section exceeds maxPushOptionsSection", func(t *testing.T) {
		// Every option stays within maxPushOption; the section as a whole does not.
		section := make([]byte, 0, 300*(maxPushOption+pktLineHeaderLen)+4)
		for range 300 {
			section = append(section, pkt(strings.Repeat("x", maxPushOption-1)+"\n")...)
		}
		section = append(section, []byte("0000")...)
		if int64(len(section)) <= maxPushOptionsSection {
			t.Fatalf("test options section is %d bytes, want more than %d", len(section), maxPushOptionsSection)
		}
		input := append(append(pkts(negotiate), section...), []byte("PACK")...)
		if _, err := ReadReceivePack(bufio.NewReader(bytes.NewReader(input))); err == nil {
			t.Fatal("accepted an oversized push-options section, want error")
		}
	})
}
