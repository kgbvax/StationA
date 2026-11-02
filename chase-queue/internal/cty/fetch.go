package cty

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"time"
)

// DefaultSource is AD1C's "big" country files (cty.dat with the full
// exception list, cty.csv with the DXCC entity numbers).
const DefaultSource = "https://www.country-files.com/bigcty/"

// Fetcher keeps <dir>/cty.dat and <dir>/cty.csv current.
type Fetcher struct {
	Source string // base URL; DefaultSource when ""
	HTTP   *http.Client
}

// Refresh downloads both files and, when either differs from the copy in
// dir, replaces them. The new files must parse and resolve a known call
// before they replace the old ones. It reports whether anything changed.
func (f Fetcher) Refresh(ctx context.Context, dir string) (bool, error) {
	files := map[string][]byte{}
	for _, name := range []string{"cty.dat", "cty.csv"} {
		b, err := f.get(ctx, name)
		if err != nil {
			return false, err
		}
		files[name] = b
	}
	same := true
	for name, b := range files {
		old, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil || !bytes.Equal(old, b) {
			same = false
		}
	}
	if same {
		return false, nil
	}
	tmp := map[string]string{}
	defer func() {
		for _, p := range tmp {
			os.Remove(p)
		}
	}()
	for name, b := range files {
		p := filepath.Join(dir, "."+name+".new")
		if err := os.WriteFile(p, b, 0o644); err != nil {
			return false, err
		}
		tmp[name] = p
	}
	db, err := LoadFile(tmp["cty.dat"])
	if err != nil {
		return false, fmt.Errorf("downloaded cty.dat unusable: %w", err)
	}
	if err := db.LoadNumbers(tmp["cty.csv"]); err != nil {
		return false, fmt.Errorf("downloaded cty.csv unusable: %w", err)
	}
	if e, ok := db.DXCC("DL1ABC"); !ok || e.ADIF != 230 {
		return false, errors.New("downloaded country files do not resolve DL1ABC to Germany (230)")
	}
	for _, name := range []string{"cty.csv", "cty.dat"} {
		if err := os.Rename(tmp[name], filepath.Join(dir, name)); err != nil {
			return false, err
		}
		delete(tmp, name)
	}
	return true, nil
}

func (f Fetcher) get(ctx context.Context, name string) ([]byte, error) {
	src := f.Source
	if src == "" {
		src = DefaultSource
	}
	hc := f.HTTP
	if hc == nil {
		hc = &http.Client{Timeout: time.Minute}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, src+name, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "chase-queue (StationA)")
	res, err := hc.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetching %s: %w", name, err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("fetching %s: %s", name, res.Status)
	}
	b, err := io.ReadAll(io.LimitReader(res.Body, 8<<20))
	if err != nil {
		return nil, fmt.Errorf("fetching %s: %w", name, err)
	}
	return b, nil
}
