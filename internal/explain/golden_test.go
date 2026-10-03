package explain_test

import (
	"bytes"
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/DanilaZanin/argocd-sync-explain/internal/explain"
	"github.com/DanilaZanin/argocd-sync-explain/internal/model"
	"github.com/DanilaZanin/argocd-sync-explain/internal/render"
)

var update = flag.Bool("update", false, "rewrite golden files")

// Snapshots in testdata/snapshots are real collector output captured from kind
// clusters with real Argo CD controllers (see test/e2e, ASE_E2E_RECORD). Each one is
// explained and compared with a golden text and a golden JSON file. Time is fixed
// to the snapshot's own Now, so the output is deterministic.
func TestGolden(t *testing.T) {
	files, err := filepath.Glob("testdata/snapshots/*.json")
	if err != nil {
		t.Fatal(err)
	}
	if len(files) == 0 {
		t.Fatal("no snapshots in testdata/snapshots")
	}
	for _, f := range files {
		name := strings.TrimSuffix(filepath.Base(f), ".json")
		t.Run(name, func(t *testing.T) {
			b, err := os.ReadFile(f)
			if err != nil {
				t.Fatal(err)
			}
			var s model.Snapshot
			if err := json.Unmarshal(b, &s); err != nil {
				t.Fatal(err)
			}
			rep := explain.Explain(&s)

			var text bytes.Buffer
			render.Text(&text, rep)
			var js bytes.Buffer
			if err := render.JSON(&js, rep); err != nil {
				t.Fatal(err)
			}
			check(t, filepath.Join("testdata", "golden", name+".txt"), text.Bytes())
			check(t, filepath.Join("testdata", "golden", name+".json"), js.Bytes())
		})
	}
}

func check(t *testing.T, path string, got []byte) {
	t.Helper()
	if *update {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, got, 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("missing golden file %s (run go test ./internal/explain -update): %v", path, err)
	}
	if !bytes.Equal(got, want) {
		t.Errorf("%s differs from the golden file; run go test ./internal/explain -update and review the diff\n--- got ---\n%s", path, got)
	}
}
