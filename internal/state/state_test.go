package state

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestMissingFileGivesDefaults(t *testing.T) {
	f, err := Load(filepath.Join(t.TempDir(), "nope.json"))
	if err != nil || !f.Display.LineNumbers || !f.Display.Wrap || !f.Display.LevelColours || f.Logs == nil {
		t.Fatalf("%+v %v", f, err)
	}
}

func TestUpdateRoundTripAndMerge(t *testing.T) {
	p := filepath.Join(t.TempDir(), ".logview.json")
	if err := Update(p, func(f *File) {
		f.Display.LineNumbers = false
		f.Logs["/a.log"] = &Rules{Filters: []string{"(?i)error"}, Subs: []string{"/x/y/"}}
	}); err != nil {
		t.Fatal(err)
	}
	// A second viewer updating another log keeps the first entry.
	if err := Update(p, func(f *File) { f.Logs["/b.log"] = &Rules{Cols: ".level"} }); err != nil {
		t.Fatal(err)
	}
	f, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	if f.Display.LineNumbers || f.Logs["/a.log"].Filters[0] != "(?i)error" || f.Logs["/b.log"].Cols != ".level" {
		t.Fatalf("%+v", f)
	}
	// Empty rule sets are not stored.
	Update(p, func(f *File) { f.Logs["/a.log"] = &Rules{Record: "auto"} })
	if f, _ = Load(p); f.Logs["/a.log"] != nil {
		t.Fatal("empty rules kept")
	}
	matches, _ := filepath.Glob(filepath.Join(filepath.Dir(p), ".logview-state-*"))
	if len(matches) != 0 {
		t.Fatalf("temp files left: %v", matches)
	}
}

func TestCorruptFileIsAnErrorAndNotOverwritten(t *testing.T) {
	p := filepath.Join(t.TempDir(), ".logview.json")
	os.WriteFile(p, []byte("{not json"), 0o644)
	if _, err := Load(p); err == nil {
		t.Fatal("expected error")
	}
	if err := Update(p, func(*File) {}); err == nil {
		t.Fatal("Update must refuse to overwrite a corrupt file")
	}
	if b, _ := os.ReadFile(p); string(b) != "{not json" {
		t.Fatal("corrupt file was overwritten")
	}
}

func TestPruneKeepsMostRecent(t *testing.T) {
	p := filepath.Join(t.TempDir(), ".logview.json")
	now := time.Now()
	Update(p, func(f *File) {
		for i := 0; i < maxLogs+10; i++ {
			f.Logs[fmt.Sprint(i)] = &Rules{Filters: []string{"x"}, LastUsed: now.Add(time.Duration(i) * time.Second)}
		}
	})
	f, _ := Load(p)
	if len(f.Logs) != maxLogs || f.Logs["0"] != nil || f.Logs[fmt.Sprint(maxLogs+9)] == nil {
		t.Fatalf("kept %d", len(f.Logs))
	}
}
