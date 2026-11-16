package ipc

import (
	"bufio"
	"bytes"
	"errors"
	"io"
	"strings"
	"syscall"
	"testing"
)

// umaskForTest sets the process umask and returns the previous one.
func umaskForTest(mask int) int {
	umaskMu.Lock()
	defer umaskMu.Unlock()
	return syscall.Umask(mask)
}

func TestReadMessage(t *testing.T) {
	const limit = 16
	cases := []struct {
		name  string
		input string
		want  []string
		err   error
	}{
		{"una línea", "abc\n", []string{"abc"}, io.EOF},
		{"varias líneas", "a\n\nbc\n", []string{"a", "", "bc"}, io.EOF},
		{"límite exacto", strings.Repeat("x", limit) + "\nok\n", []string{strings.Repeat("x", limit), "ok"}, io.EOF},
		{"un byte más", strings.Repeat("x", limit+1) + "\nok\n", nil, errTooLarge},
		{"enorme sin salto", strings.Repeat("x", 10*limit), nil, errTooLarge},
		{"sin salto final", "abc", nil, io.EOF},
		{"vacío", "", nil, io.EOF},
		{"segunda línea grande", "ok\n" + strings.Repeat("x", limit+1) + "\n", []string{"ok"}, errTooLarge},
	}
	for _, tc := range cases {
		// A buffer smaller than the limit forces the accumulation path.
		for _, size := range []int{16, 4096} {
			r := bufio.NewReaderSize(strings.NewReader(tc.input), size)
			var got []string
			var err error
			for {
				var msg []byte
				if msg, err = readMessage(r, limit); err != nil {
					break
				}
				got = append(got, string(msg))
			}
			if !errors.Is(err, tc.err) {
				t.Errorf("%s (buffer %d): err = %v, se esperaba %v", tc.name, size, err, tc.err)
			}
			if strings.Join(got, "|") != strings.Join(tc.want, "|") || len(got) != len(tc.want) {
				t.Errorf("%s (buffer %d): mensajes = %q, se esperaban %q", tc.name, size, got, tc.want)
			}
		}
	}
}

func TestDecodeArgs(t *testing.T) {
	type args struct {
		Watts *int `json:"watts"`
	}
	for _, raw := range []string{"", "null", "  null ", "{}"} {
		var a args
		if err := DecodeArgs([]byte(raw), &a); err != nil || a.Watts != nil {
			t.Errorf("%q: err = %v, watts = %v", raw, err, a.Watts)
		}
	}
	var a args
	if err := DecodeArgs([]byte(`{"watts":120}`), &a); err != nil || a.Watts == nil || *a.Watts != 120 {
		t.Fatalf("args válidos: err = %v", err)
	}
	for _, raw := range []string{`{"watts":120,"x":1}`, `{"watts":"120"}`, `{"watts":1.5}`, `[120]`, `{"watts":120} {}`, `{"watts":1e99}`} {
		var a args
		if err := DecodeArgs([]byte(raw), &a); err == nil {
			t.Errorf("%q: se esperaba un error", raw)
		}
	}
}

// FuzzReadMessage feeds arbitrary streams to the framing and the request
// decoder: no panic, no message over the limit, no newline inside a message.
func FuzzReadMessage(f *testing.F) {
	for _, seed := range []string{
		`{"v":1,"id":"1","cmd":"status"}` + "\n",
		`{"v":1,"id":"1","cmd":"set_eco_watts","args":{"watts":123}}` + "\n" + `null` + "\n",
		"\n\n\n",
		strings.Repeat("a", 300),
		"\x00\xff\n{\"v\":1}\n",
		`{"v":1,"id":"` + strings.Repeat("x", 100) + `","cmd":"status"}` + "\n",
	} {
		f.Add([]byte(seed))
	}
	const limit = 128
	f.Fuzz(func(t *testing.T, data []byte) {
		r := bufio.NewReaderSize(bytes.NewReader(data), 16)
		consumed := 0
		for {
			msg, err := readMessage(r, limit)
			if err != nil {
				if !errors.Is(err, errTooLarge) && !errors.Is(err, io.EOF) {
					t.Fatalf("error inesperado: %v", err)
				}
				return
			}
			if len(msg) > limit {
				t.Fatalf("mensaje de %d bytes con límite %d", len(msg), limit)
			}
			if bytes.IndexByte(msg, '\n') >= 0 {
				t.Fatalf("salto de línea dentro del mensaje %q", msg)
			}
			if !bytes.Equal(msg, data[consumed:consumed+len(msg)]) || data[consumed+len(msg)] != '\n' {
				t.Fatalf("el mensaje %q no coincide con la entrada en %d", msg, consumed)
			}
			consumed += len(msg) + 1

			req, bad := decodeRequest(msg)
			if bad == nil && (req.V != Version || len(req.ID) > MaxIDLen || !knownCmd(req.Cmd)) {
				t.Fatalf("request inválida aceptada: %+v", req)
			}
			if bad != nil && bad.Code != CodeBadRequest && bad.Code != CodeUnknownCmd {
				t.Fatalf("código inesperado %q", bad.Code)
			}
		}
	})
}
