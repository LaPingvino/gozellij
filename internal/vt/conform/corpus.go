package conform

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// Dir is where the corpus lives, relative to this package.
const Dir = "testdata"

// LoadCases reads every case in a directory.
//
// A case is <name>.in plus <name>.want. The size is in the .in file's first comment line, as
// `# size <cols>x<rows>`, so that one file carries the whole case and a screen cannot be recorded
// at a different size from the one it is compared at.
func LoadCases(dir string) ([]Case, error) {
	names, err := filepath.Glob(filepath.Join(dir, "*.in"))
	if err != nil {
		return nil, err
	}
	if len(names) == 0 {
		// An empty corpus passing every check is the failure this whole package is about.
		return nil, fmt.Errorf("no cases in %s", dir)
	}
	var cases []Case
	for _, path := range names {
		c, err := LoadCase(path)
		if err != nil {
			return nil, err
		}
		cases = append(cases, c)
	}
	return cases, nil
}

// LoadCase reads one .in file.
func LoadCase(path string) (Case, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return Case{}, err
	}
	text := string(b)
	cols, rows, err := size(text)
	if err != nil {
		return Case{}, fmt.Errorf("%s: %w", path, err)
	}
	input, err := Unescape(text)
	if err != nil {
		return Case{}, fmt.Errorf("%s: %w", path, err)
	}
	raw, err := included(text, filepath.Dir(path))
	if err != nil {
		return Case{}, fmt.Errorf("%s: %w", path, err)
	}
	input = append(input, raw...)
	name := strings.TrimSuffix(filepath.Base(path), ".in")
	return Case{Name: name, Cols: cols, Rows: rows, Input: input}, nil
}

// WantPath is where a case's recording lives.
func WantPath(dir, name string) string { return filepath.Join(dir, name+".want") }

func size(text string) (cols, rows int, err error) {
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		rest, ok := strings.CutPrefix(line, "# size ")
		if !ok {
			continue
		}
		c, r, ok := strings.Cut(strings.TrimSpace(rest), "x")
		if !ok {
			return 0, 0, fmt.Errorf("size must be written <cols>x<rows>")
		}
		if cols, err = strconv.Atoi(c); err != nil {
			return 0, 0, fmt.Errorf("size columns %q: %w", c, err)
		}
		if rows, err = strconv.Atoi(r); err != nil {
			return 0, 0, fmt.Errorf("size rows %q: %w", r, err)
		}
		if cols <= 0 || rows <= 0 {
			return 0, 0, fmt.Errorf("size %dx%d is not a screen", cols, rows)
		}
		return cols, rows, nil
	}
	return 0, 0, fmt.Errorf("no `# size <cols>x<rows>` line")
}

// included appends the bytes of any `# include <file>` line.
//
// A capture of a real program is thousands of bytes of escape sequences, and escaping those into a
// .in file would produce something no one can review and a diff no one can read. So the readable
// header stays in the .in file and the payload sits beside it as raw bytes - which is also exactly
// what a terminal received, with nothing in between to get wrong.
func included(text, dir string) ([]byte, error) {
	var out []byte
	for _, line := range strings.Split(text, "\n") {
		rest, ok := strings.CutPrefix(strings.TrimSpace(line), "# include ")
		if !ok {
			continue
		}
		name := strings.TrimSpace(rest)
		if name == "" || strings.Contains(name, "/") {
			// A plain name beside the case. A path would make a corpus that only works from
			// one directory, and "../" would make it a way to read anything.
			return nil, fmt.Errorf("include takes a file name beside the case, not %q", name)
		}
		b, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			return nil, err
		}
		out = append(out, b...)
	}
	return out, nil
}
