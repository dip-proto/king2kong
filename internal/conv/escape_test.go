package conv

import (
	"path/filepath"
	"strings"
	"testing"
)

// TestForeignApplication checks that a program passing its application to
// outside code is refused.
// That code may add flags or parse with it, so converting the program alone
// would be wrong.
func TestForeignApplication(t *testing.T) {
	dir, err := filepath.Abs(filepath.Join("testdata", "foreignapp"))
	if err != nil {
		t.Fatal(err)
	}
	prog, err := Load(dir, "./...")
	if err != nil {
		t.Fatal(err)
	}
	res, err := Convert(prog, Options{Compat: compatFS(t)})
	if err == nil {
		t.Fatal("converted a program whose application escapes")
	}
	if len(res.Files) != 0 || len(res.NewFiles) != 0 {
		t.Errorf("files were changed: %d, %d", len(res.Files), len(res.NewFiles))
	}
	found := false
	for _, is := range res.Issues {
		if strings.Contains(is.Reason, "fmt.Fprintln") {
			found = true
		}
	}
	if !found {
		t.Errorf("the escape is not reported: %v", res.Issues)
	}
}
