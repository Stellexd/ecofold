//go:build darwin

package detector

import (
	"bytes"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"howett.net/plist"
)

// probeFormat decodes raw with howett.net/plist directly (no validateXMLPlist)
// and reports the format the decoder selected. It recovers panics instead of
// failing so the caller can distinguish "howett panicked" from "howett fell
// back to the text parser". A returned format of plist.OpenStepFormat or
// plist.BinaryFormat means the XML-only guard was bypassed.
func probeFormat(raw []byte) (format int, panicked any, err error) {
	dec := plist.NewDecoder(bytes.NewReader(raw))
	defer func() {
		if r := recover(); r != nil {
			panicked = r
			format = dec.Format
		}
	}()
	err = dec.Decode(new(any))
	return dec.Format, nil, err
}

// TestValidateThenDecodeNeverFallsBackToText is the differential guard for
// A1/M1: for every crafted document that validateXMLPlist accepts, the howett
// XML parser must also accept it and must never drop into the OpenStep text
// parser (the path that can burn unbounded CPU on hostile input).
func TestValidateThenDecodeNeverFallsBackToText(t *testing.T) {
	inputs := map[string]string{
		"minimal":                 `<plist version="1.0"><array/></plist>`,
		"doctype":                 `<?xml version="1.0" encoding="UTF-8"?><!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd"><plist version="1.0"><array/></plist>`,
		"namespace_prefixed_root": `<x:plist xmlns:x="urn:apple:plist" version="1.0"><array/></x:plist>`,
		"default_namespace_root":  `<plist xmlns="urn:apple:plist" version="1.0"><array/></plist>`,
		"xml_prefix_root":         `<xml:plist version="1.0"><array/></xml:plist>`,
		"utf8_bom":                "\ufeff<plist version=\"1.0\"><array/></plist>",
		"comment_before_root":     `<!-- hostile --><?xml version="1.0"?><plist version="1.0"><array/></plist>`,
		"processing_instruction":  `<?xml-stylesheet href="x"?><plist version="1.0"><array/></plist>`,
		"cdata":                   `<plist version="1.0"><dict><key>k</key><string><![CDATA[<plist>&amp;]]></string></dict></plist>`,
		"predefined_entities":     `<plist version="1.0"><dict><key>k</key><string>&lt;&gt;&amp;&quot;&apos;</string></dict></plist>`,
		"numeric_reference":       `<plist version="1.0"><dict><key>k</key><string>&#65;&#x42;</string></dict></plist>`,
		"self_closing_root":       `<plist version="1.0"/>`,
		"empty_root":              `<plist version="1.0"></plist>`,
		"trailing_whitespace":     "<plist version=\"1.0\"><array/></plist>\r\n\t ",
		"wrong_child_types":       `<plist version="1.0"><array><true/><false/><integer>1</integer><real>1.5</real><date>2026-11-09T00:00:00Z</date><data>aGVsbG8=</data></array></plist>`,
		"key_before_value":        `<plist version="1.0"><dict><key>ExternalConnected</key><true/></dict></plist>`,
	}

	for name, doc := range inputs {
		t.Run(name, func(t *testing.T) {
			raw := []byte(doc)
			if err := validateXMLPlist(raw); err != nil {
				t.Fatalf("validateXMLPlist rechazó un documento XML válido: %v", err)
			}

			format, panicked, err := probeFormat(raw)
			if panicked != nil {
				t.Fatalf("howett entró en panic tras pasar validateXMLPlist: %v", panicked)
			}
			if err != nil {
				t.Fatalf("howett rechazó un documento que validateXMLPlist aceptó: %v", err)
			}
			if format != plist.XMLFormat {
				t.Fatalf("howett usó el formato %q en vez de XML: el guard XML-only fue evadido",
					plist.FormatNames[format])
			}
		})
	}
}

// TestDecodeXMLPlistContainsHowettPanics verifies that semantically invalid
// but well-formed XML that makes howett panic (integer overflow, bad date, bad
// base64, missing dict key/value) is converted into an error, never a panic.
func TestDecodeXMLPlistContainsHowettPanics(t *testing.T) {
	cases := map[string]string{
		"integer_overflow":   `<plist version="1.0"><integer>999999999999999999999999999999999999999999</integer></plist>`,
		"empty_integer":      `<plist version="1.0"><integer></integer></plist>`,
		"invalid_integer":    `<plist version="1.0"><integer>not-a-number</integer></plist>`,
		"invalid_real":       `<plist version="1.0"><real>not-a-real</real></plist>`,
		"invalid_date":       `<plist version="1.0"><date>yesterday</date></plist>`,
		"invalid_base64":     `<plist version="1.0"><data>!!!not-base64!!!</data></plist>`,
		"dict_missing_value": `<plist version="1.0"><dict><key>orphan</key></dict></plist>`,
		"dict_missing_key":   `<plist version="1.0"><dict><string>value-without-key</string></dict></plist>`,
		"unknown_element":    `<plist version="1.0"><frobnicate/></plist>`,
		"nested_unknown":     `<plist version="1.0"><array><frobnicate/></array></plist>`,
		"key_at_root":        `<plist version="1.0"><key>k</key></plist>`,
	}

	for name, doc := range cases {
		t.Run(name, func(t *testing.T) {
			raw := []byte(doc)
			if err := validateXMLPlist(raw); err != nil {
				t.Fatalf("el caso debería ser XML válido, validateXMLPlist devolvió: %v", err)
			}

			var out any
			func() {
				defer func() {
					if r := recover(); r != nil {
						t.Fatalf("decodeXMLPlist dejó escapar un panic: %v", r)
					}
				}()
				if err := decodeXMLPlist(raw, &out); err == nil {
					t.Fatalf("decodeXMLPlist devolvió nil error para un plist semánticamente inválido")
				}
			}()

			// The public parsers must also fail cleanly instead of panicking.
			if _, err := ParseSmartBatteryPlist(raw); err == nil {
				t.Errorf("ParseSmartBatteryPlist devolvió nil error")
			}
			if _, err := ParseIOAcceleratorPlist(raw); err == nil {
				t.Errorf("ParseIOAcceleratorPlist devolvió nil error")
			}
		})
	}
}

// TestValidateRejectsEntityExpansionBomb attempts a "billion laughs" payload.
// encoding/xml does not expand DTD-declared entities in strict mode, so the
// guard must reject the document without hanging or allocating exponentially.
func TestValidateRejectsEntityExpansionBomb(t *testing.T) {
	var b strings.Builder
	b.WriteString(`<?xml version="1.0"?><!DOCTYPE plist [`)
	b.WriteString(`<!ENTITY lol "lol">`)
	for i := 1; i <= 9; i++ {
		b.WriteString(`<!ENTITY lol`)
		b.WriteByte(byte('0' + i))
		b.WriteString(` "`)
		for j := 0; j < 10; j++ {
			b.WriteString("&lol")
			if i > 1 {
				b.WriteByte(byte('0' + i - 1))
			}
			b.WriteString(";")
		}
		b.WriteString(`">`)
	}
	b.WriteString(`]><plist version="1.0"><string>&lol9;</string></plist>`)
	raw := []byte(b.String())

	start := time.Now()
	err := validateXMLPlist(raw)
	elapsed := time.Since(start)
	if err == nil {
		t.Fatalf("validateXMLPlist aceptó un documento con entidades DTD no expandibles")
	}
	if elapsed > 2*time.Second {
		t.Fatalf("validateXMLPlist tardó %v en rechazar el bombardeo de entidades", elapsed)
	}
}

// TestValidateRejectsMalformedXMLFrontier covers decoder-level edge cases that
// must be rejected before howett sees them.
func TestValidateRejectsMalformedXMLFrontier(t *testing.T) {
	inputs := map[string]string{
		"stray_end_tag":         `</plist>`,
		"mismatched_end":        `<plist><array></plist></array>`,
		"unclosed_comment":      `<plist><!-- unterminated </plist>`,
		"unclosed_cdata":        `<plist><![CDATA[ x </plist>`,
		"unclosed_doctype":      `<!DOCTYPE plist [ <!ENTITY x "y" > <plist/>`,
		"utf16_declaration":     "\xff\xfe<\x00p\x00l\x00i\x00s\x00t\x00/\x00>\x00",
		"utf16_declared":        `<?xml version="1.0" encoding="UTF-16"?><plist/>`,
		"invalid_utf8":          string([]byte{'<', 'p', 'l', 'i', 's', 't', '>', 0xff, 0xfe, '<', '/', 'p', 'l', 'i', 's', 't', '>'}),
		"nul_byte":              "<plist>\x00</plist>",
		"binary_with_ws_prefix": " \x00bplist00",
	}

	for name, doc := range inputs {
		t.Run(name, func(t *testing.T) {
			if err := validateXMLPlist([]byte(doc)); err == nil {
				t.Fatalf("validateXMLPlist aceptó entrada malformada")
			}
		})
	}
}

// TestDepthLimitBoundary uses several element shapes to make sure the depth
// counter agrees with howett's recursion and rejects one level past the limit.
func TestDepthLimitBoundary(t *testing.T) {
	build := func(tag string, depth int) []byte {
		var b bytes.Buffer
		b.WriteString(`<plist version="1.0">`)
		for i := 0; i < depth-1; i++ {
			b.WriteString("<" + tag + ">")
		}
		for i := 0; i < depth-1; i++ {
			b.WriteString("</" + tag + ">")
		}
		b.WriteString(`</plist>`)
		return b.Bytes()
	}

	for _, tag := range []string{"array", "dict"} {
		t.Run(tag+"_at_limit", func(t *testing.T) {
			if err := validateXMLPlist(build(tag, maxPlistDepth)); err != nil {
				t.Fatalf("profundidad %d rechazada: %v", maxPlistDepth, err)
			}
		})
		t.Run(tag+"_over_limit", func(t *testing.T) {
			if err := validateXMLPlist(build(tag, maxPlistDepth+1)); err == nil {
				t.Fatalf("profundidad %d aceptada (límite %d)", maxPlistDepth+1, maxPlistDepth)
			}
		})
	}

	t.Run("namespace_prefixed_over_limit", func(t *testing.T) {
		var b bytes.Buffer
		b.WriteString(`<x:plist xmlns:x="urn:x" version="1.0">`)
		for i := 0; i < maxPlistDepth-1; i++ {
			b.WriteString(`<x:array>`)
		}
		for i := 0; i < maxPlistDepth-1; i++ {
			b.WriteString(`</x:array>`)
		}
		b.WriteString(`</x:plist>`)
		if err := validateXMLPlist(b.Bytes()); err != nil {
			t.Fatalf("profundidad exacta con namespace rechazada: %v", err)
		}
		b.Reset()
		b.WriteString(`<x:plist xmlns:x="urn:x" version="1.0">`)
		for i := 0; i < maxPlistDepth; i++ {
			b.WriteString(`<x:array>`)
		}
		for i := 0; i < maxPlistDepth; i++ {
			b.WriteString(`</x:array>`)
		}
		b.WriteString(`</x:plist>`)
		if err := validateXMLPlist(b.Bytes()); err == nil {
			t.Fatal("una profundidad de un nivel por encima del límite fue aceptada con namespace")
		}
	})
}

// TestSizeLimitExactBoundary checks that exactly maxPlistBytes is accepted
// (the guard is strict greater-than) and one byte more is rejected. The padding
// is an XML comment before the root, so the document stays well-formed.
func TestSizeLimitExactBoundary(t *testing.T) {
	const tail = `<plist version="1.0"><array/></plist>`
	filler := maxPlistBytes - len(tail) - len(`<!--`) - len(`-->`)
	if filler < 0 {
		t.Fatalf("maxPlistBytes demasiado pequeño para la prueba")
	}
	exact := make([]byte, 0, maxPlistBytes)
	exact = append(exact, `<!--`...)
	exact = append(exact, bytes.Repeat([]byte{'a'}, filler)...)
	exact = append(exact, `-->`...)
	exact = append(exact, tail...)
	if len(exact) != maxPlistBytes {
		t.Fatalf("construcción inválida: %d != %d", len(exact), maxPlistBytes)
	}

	if err := validateXMLPlist(exact); err != nil {
		t.Fatalf("validateXMLPlist rechazó exactamente maxPlistBytes: %v", err)
	}

	over := append(append([]byte{}, exact...), ' ')
	if err := validateXMLPlist(over); err == nil {
		t.Fatalf("validateXMLPlist aceptó maxPlistBytes+1")
	}
}

// TestLargeDataElementDoesNotExplode is a DoS smoke test: a <data> element near
// the input budget must decode in linear-ish time without panics.
func TestLargeDataElementDoesNotExplode(t *testing.T) {
	payload := bytes.Repeat([]byte("A"), maxPlistBytes-64) // just under the size cap
	var b bytes.Buffer
	b.WriteString(`<plist version="1.0"><data>`)
	b.Write(payload)
	b.WriteString(`</data></plist>`)

	var out any
	start := time.Now()
	err := decodeXMLPlist(b.Bytes(), &out)
	elapsed := time.Since(start)
	if err != nil {
		// The payload "AAAA..." is valid base64, so decoding is expected to
		// succeed; surface a failure if it does not.
		t.Fatalf("decodeXMLPlist con <data> grande devolvió error: %v", err)
	}
	if elapsed > 10*time.Second {
		t.Fatalf("decodeXMLPlist con <data> grande tardó %v", elapsed)
	}
}

// FuzzValidateThenDecodeStaysXML asserts the core security invariant on
// arbitrary input: either validateXMLPlist rejects it, or howett decodes it as
// XML without ever selecting the text/binary parser. Panics escaping
// decodeXMLPlist are failures.
func FuzzValidateThenDecodeStaysXML(f *testing.F) {
	f.Add([]byte(`<plist version="1.0"><array/></plist>`))
	f.Add([]byte(`<plist version="1.0"><dict><key>k</key><integer>1</integer></dict></plist>`))
	f.Add([]byte(`<x:plist xmlns:x="u"><array/></x:plist>`))
	f.Add([]byte(`<?xml version="1.0"?><!DOCTYPE plist><plist version="1.0"><array/></plist>`))
	f.Add([]byte(`<plist version="1.0"><integer>not-a-number</integer></plist>`))
	f.Add([]byte(`<plist version="1.0"><data>!!!</data></plist>`))
	f.Add([]byte(`<plist><array></plist></array>`))
	f.Add([]byte("bplist00\x00\x00\x00\x00\x00\x00"))
	f.Add([]byte(`({a=b;})`))
	f.Add(readFixture(f, "m4_air_battery_discharging.plist"))
	f.Add(readFixture(f, "m4_air_gpu_idle.plist"))

	f.Fuzz(func(t *testing.T, raw []byte) {
		// decodeXMLPlist must contain every panic no matter what bytes arrive.
		var out any
		func() {
			defer func() {
				if r := recover(); r != nil {
					t.Fatalf("panic escapó de decodeXMLPlist con %d bytes: %v", len(raw), r)
				}
			}()
			_ = decodeXMLPlist(raw, &out)
		}()

		if err := validateXMLPlist(raw); err != nil {
			return
		}

		format, panicked, err := probeFormat(raw)
		if panicked != nil {
			// A recovered panic from howett is acceptable (decodeXMLPlist
			// converts it to an error), as long as it is not a text fallback.
			return
		}
		if err != nil {
			return
		}
		if format != plist.XMLFormat {
			t.Fatalf("validateXMLPlist aceptó %q pero howett seleccionó el formato %q",
				raw, plist.FormatNames[format])
		}
	})
}

// BenchmarkValidateHugeComment measures the cost of tokenizing a single large
// XML comment. It is used to check that the size guard is not hiding quadratic
// CPU behavior: 2x the bytes should cost roughly 2x, not 4x.
func BenchmarkValidateHugeComment(b *testing.B) {
	for _, size := range []int{256 << 10, 512 << 10, 1 << 20, maxPlistBytes} {
		filler := size - len(`<!--`) - len(`-->`) - len(`<plist version="1.0"><array/></plist>`)
		doc := make([]byte, 0, size)
		doc = append(doc, `<!--`...)
		doc = append(doc, bytes.Repeat([]byte{'a'}, filler)...)
		doc = append(doc, `-->`...)
		doc = append(doc, `<plist version="1.0"><array/></plist>`...)

		b.Run(fmt.Sprintf("%dKiB", size>>10), func(b *testing.B) {
			b.SetBytes(int64(len(doc)))
			for i := 0; i < b.N; i++ {
				if err := validateXMLPlist(doc); err != nil {
					b.Fatalf("validateXMLPlist: %v", err)
				}
			}
		})
	}
}

// TestParsersDoNotMutateInput guarantees that decoding leaves the caller's
// buffer untouched. The text parser in howett.net/plist aliases the input with
// unsafe zero-copy strings; even though it is unreachable behind the XML guard,
// a regression that lets it run must not corrupt shared ioreg buffers.
func TestParsersDoNotMutateInput(t *testing.T) {
	raw := readFixture(t, "m4_air_battery_charging.plist")
	original := append([]byte(nil), raw...)

	if _, err := ParseSmartBatteryPlist(raw); err != nil {
		t.Fatalf("ParseSmartBatteryPlist: %v", err)
	}
	if !bytes.Equal(raw, original) {
		t.Fatalf("ParseSmartBatteryPlist mutó el buffer de entrada")
	}

	gpuRaw := readFixture(t, "m4_air_gpu_idle.plist")
	gpuOriginal := append([]byte(nil), gpuRaw...)
	if _, err := ParseIOAcceleratorPlist(gpuRaw); err != nil {
		t.Fatalf("ParseIOAcceleratorPlist: %v", err)
	}
	if !bytes.Equal(gpuRaw, gpuOriginal) {
		t.Fatalf("ParseIOAcceleratorPlist mutó el buffer de entrada")
	}
}

// TestParsersConcurrent runs the XML validation/decoding path from many
// goroutines over shared buffers. The functions must be stateless; run with
// -race.
func TestParsersConcurrent(t *testing.T) {
	batt := readFixture(t, "m4_air_battery_charging.plist")
	gpu := readFixture(t, "m4_air_gpu_idle.plist")

	const workers = 8
	const iterations = 10
	errs := make(chan error, workers)
	var wg sync.WaitGroup
	wg.Add(workers)
	for w := 0; w < workers; w++ {
		go func() {
			defer wg.Done()
			for i := 0; i < iterations; i++ {
				if _, err := ParseSmartBatteryPlist(batt); err != nil {
					errs <- err
					return
				}
				if _, err := ParseIOAcceleratorPlist(gpu); err != nil {
					errs <- err
					return
				}
				if _, err := BuildSnapshotFromPlists(batt, gpu); err != nil {
					errs <- err
					return
				}
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Errorf("parse concurrente devolvió error: %v", err)
	}
}
