package cli

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/alexknips/beadline/internal/render"
	"github.com/alexknips/beadline/internal/roadmap"
)

const renderHelp = `Usage: beadline render [flags]

Hidden: 'beadline' writes the page itself. This renders an existing
roadmap.json as the single-file HTML page.

Flags:
  --in FILE      roadmap.json to render (default roadmap.json)
  --out FILE     HTML file to write, or "-" for stdout (default roadmap.html)
  --title TEXT   page title (default Roadmap)
  -h, --help     show this help
`

// runRender reads roadmap.json and writes the single-file HTML page.
func runRender(args []string, stdout, stderr io.Writer) int {
	fs := newFlagSet()
	in := fs.String("in", "roadmap.json", "roadmap.json to render")
	out := fs.String("out", "roadmap.html", `HTML file to write, or "-" for stdout`)
	title := fs.String("title", "Roadmap", "page title")
	if err := fs.Parse(args); err != nil {
		return flagError(stdout, stderr, "render", renderHelp, err)
	}
	if fs.NArg() > 0 {
		return usageError(stderr, "render", "unexpected argument %q", fs.Arg(0))
	}

	r, err := roadmap.ReadFile(*in)
	if err != nil {
		fmt.Fprintf(stderr, "beadline render: %v\n", err)
		return ExitFailure
	}
	var page bytes.Buffer
	if err := render.Render(&page, r, render.Options{Title: *title, Version: version()}); err != nil {
		fmt.Fprintf(stderr, "beadline render: %v\n", err)
		return ExitFailure
	}
	if *out == "-" {
		_, err = stdout.Write(page.Bytes())
	} else {
		err = writeFile(*out, page.Bytes())
	}
	if err != nil {
		fmt.Fprintf(stderr, "beadline render: %v\n", err)
		return ExitFailure
	}
	return ExitOK
}

// writeFile replaces path through a temporary file in the same directory, so
// a web server never serves half a page.
func writeFile(path string, data []byte) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), ".beadline-*.tmp")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmp.Name(), 0o644); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}
