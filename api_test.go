package websocket

import (
	"bytes"
	"flag"
	"go/ast"
	"go/build"
	"go/doc"
	"go/parser"
	"go/printer"
	"go/token"
	"os"
	"path/filepath"
	"testing"
)

var updateAPI = flag.Bool("update-api", false, "rewrite api/v0.txt from the current exported API")

// apiPackages are the packages whose exported API is recorded in api/v0.txt,
// with the GOOS each is read under.
var apiPackages = []struct{ dir, goos string }{
	{".", "linux"},
	{"wsjson", "linux"},
	{"browser", "js"},
}

// TestAPI fails when the exported API differs from api/v0.txt, so every
// change to it is a reviewed diff of that file. After an intended change, run
//
//	go test -run '^TestAPI$' -update-api .
//
// and commit the result.
func TestAPI(t *testing.T) {
	var got bytes.Buffer
	for _, p := range apiPackages {
		writePackageAPI(t, &got, p.dir, p.goos)
	}
	const golden = "api/v0.txt"
	if *updateAPI {
		if err := os.MkdirAll(filepath.Dir(golden), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(golden, got.Bytes(), 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(golden)
	if err != nil {
		t.Fatal(err)
	}
	// A Windows checkout may convert the file's line endings.
	want = bytes.ReplaceAll(want, []byte("\r\n"), []byte("\n"))
	if !bytes.Equal(got.Bytes(), want) {
		t.Errorf("exported API differs from %s; if the change is intended, run go test -run '^TestAPI$' -update-api . and review the diff", golden)
	}
}

// writePackageAPI writes the exported declarations of the package in dir, as
// built for goos, without comments or function bodies.
func writePackageAPI(t *testing.T, w *bytes.Buffer, dir, goos string) {
	t.Helper()
	ctx := build.Default
	ctx.GOOS, ctx.GOARCH = goos, "amd64"
	if goos == "js" {
		ctx.GOARCH = "wasm"
	}
	bp, err := ctx.ImportDir(dir, 0)
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	var files []*ast.File
	for _, name := range bp.GoFiles {
		f, err := parser.ParseFile(fset, filepath.Join(dir, name), nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		files = append(files, f)
	}
	pkg, err := doc.NewFromFiles(fset, files, "github.com/picatz/websocket/"+dir)
	if err != nil {
		t.Fatal(err)
	}

	w.WriteString("// " + filepath.ToSlash(filepath.Join("github.com/picatz/websocket", dir)) + ", GOOS=" + goos + "\npackage " + pkg.Name + "\n")
	write := func(node any) {
		w.WriteString("\n")
		if err := (&printer.Config{Mode: printer.UseSpaces | printer.TabIndent, Tabwidth: 8}).Fprint(w, fset, node); err != nil {
			t.Fatal(err)
		}
		w.WriteString("\n")
	}
	values := func(vs []*doc.Value) {
		for _, v := range vs {
			write(v.Decl)
		}
	}
	funcs := func(fs []*doc.Func) {
		for _, f := range fs {
			f.Decl.Body = nil
			write(f.Decl)
		}
	}
	values(pkg.Consts)
	values(pkg.Vars)
	funcs(pkg.Funcs)
	for _, typ := range pkg.Types {
		write(typ.Decl)
		values(typ.Consts)
		values(typ.Vars)
		funcs(typ.Funcs)
		funcs(typ.Methods)
	}
	w.WriteString("\n")
}
