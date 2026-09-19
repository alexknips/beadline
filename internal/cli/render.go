package cli

import (
	"bytes"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/alexknips/beadline/internal/render"
	"github.com/alexknips/beadline/internal/roadmap"
)

// runRender reads roadmap.json and writes the single-file HTML page.
func runRender(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("render", flag.ContinueOnError)
	fs.SetOutput(stderr)
	in := fs.String("in", "roadmap.json", "roadmap.json to render")
	out := fs.String("out", "roadmap.html", `HTML file to write, or "-" for stdout`)
	title := fs.String("title", "Roadmap", "page title")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return ExitOK
		}
		return ExitUsage
	}
	if fs.NArg() > 0 {
		fmt.Fprintf(stderr, "beadline render: unexpected argument %q\n", fs.Arg(0))
		return ExitUsage
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
