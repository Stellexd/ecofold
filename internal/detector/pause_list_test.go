package detector

import (
	"fmt"
	"reflect"
	"strings"
	"testing"
)

func TestParsePauseProcesses(t *testing.T) {
	got, err := ParsePauseProcesses(" Blender, obs ,,resolve*, BLENDER ")
	if err != nil {
		t.Fatalf("ParsePauseProcesses: %v", err)
	}
	if want := []string{"blender", "obs", "resolve*"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("= %v, se esperaba %v", got, want)
	}
	if got, err := ParsePauseProcesses(""); err != nil || got != nil {
		t.Fatalf("lista vacía = %v, %v", got, err)
	}
}

func TestParsePauseProcessesRejects(t *testing.T) {
	bad := []string{
		"*", "**",
		"/usr/bin/blender", `C:\x.exe`,
		"fah*", "f*", "FahCore_27", "fahclient", "folding*",
		"bl*nder",
		strings.Repeat("a", maxPausePatternLen+1),
	}
	for _, list := range bad {
		if _, err := ParsePauseProcesses(list); err == nil {
			t.Errorf("ParsePauseProcesses(%q) no devolvió error", list)
		}
	}
	var many []string
	for i := 0; i <= maxPausePatterns; i++ {
		many = append(many, fmt.Sprintf("app%d", i))
	}
	if _, err := ParsePauseProcesses(strings.Join(many, ",")); err == nil {
		t.Error("se esperaba error con más de maxPausePatterns patrones")
	}
}

func FuzzParsePauseProcesses(f *testing.F) {
	f.Add("blender,obs")
	f.Add("*,fah*")
	f.Add("a*,b,,c*")
	f.Fuzz(func(t *testing.T, list string) {
		patterns, err := ParsePauseProcesses(list)
		if err != nil {
			return
		}
		for _, p := range patterns {
			for _, fah := range []string{"fahcore_27", "fahclient", "fah-client", "foldingathome"} {
				if matchPattern([]string{p}, fah) {
					t.Fatalf("el patrón %q aceptado coincide con %q", p, fah)
				}
			}
		}
	})
}
