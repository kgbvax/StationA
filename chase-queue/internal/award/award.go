// Package award is chase-queue's generic award model. An award is a
// definition (key + slice + confirmation rule), not Go code: DXCC per band is
// the built-in one, a field award such as WAS comes from a TOML file, and a
// running contest is wrapped as one more award (contest.go). The evaluator
// (eval.go) answers, for a candidate station, which slots of which awards it
// would fill and how each slot stands in the log. It does no I/O of its own
// beyond reading the definition files it is pointed at.
package award

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"

	"github.com/BurntSushi/toml"
)

// Kind is how an award derives the key of a slot.
type Kind string

const (
	// KindDXCC keys by DXCC entity (the ADIF number from cty.csv).
	KindDXCC Kind = "dxcc"
	// KindField keys by a QSO field (STATE) within an allowed set of entities.
	KindField Kind = "field"
	// KindContest is the running contest's multipliers (built in, never a file).
	KindContest Kind = "contest"
	// kindReference (SOTA, WWFF, POTA) is deferred: the loader rejects it.
	kindReference Kind = "reference"
)

// Rule says what satisfies a slot.
type Rule string

const (
	// RuleWorked: a logged QSO is enough.
	RuleWorked Rule = "worked"
	// RuleConfirmed: a confirmed QSO (QSL, LoTW, eQSL or Club Log) is needed.
	RuleConfirmed Rule = "confirmed"
)

// Status is the state of one slot in the log.
type Status string

const (
	Unworked          Status = "unworked"
	WorkedUnconfirmed Status = "worked-unconfirmed"
	Confirmed         Status = "confirmed"
	// Unknown: the slot cannot be told (no log yet, the station's key is not
	// known). It is never "not needed".
	Unknown Status = "unknown"
)

// Mode classes, as station-log's mode_class.
const (
	ClassCW    = "cw"
	ClassPhone = "phone"
	ClassData  = "data"
)

// Built-in award codes.
const (
	CodeDXCC    = "dxcc"
	CodeContest = "contest"
)

// Definition is one award. TOML files hold [[award]] tables of these.
type Definition struct {
	// Code is the short unique name goals refer to (case-insensitive, stored
	// as written; "WAS", "WAS-20-CW").
	Code string `toml:"code"`
	Name string `toml:"name"`
	Kind Kind   `toml:"kind"`
	// Rule defaults to "confirmed".
	Rule Rule `toml:"rule"`
	// Field is the QSO field a field award keys by; only "STATE".
	Field string `toml:"field,omitempty"`
	// DXCC lists the entities (ADIF numbers) a field award applies to; empty
	// = every entity. A station of another entity is not part of the award.
	DXCC []int `toml:"dxcc,omitempty"`
	// Items lists the allowed keys of a field award; empty = any value.
	Items []string `toml:"items,omitempty"`
	// Bands restricts the award to these bands; empty = every band.
	Bands []string `toml:"bands,omitempty"`
	// Modes restricts the award to these mode classes (cw, phone, data);
	// empty = any mode.
	Modes []string `toml:"modes,omitempty"`
	// PerBand (default true) makes each band its own slot; false counts a
	// key once over all bands.
	PerBand *bool `toml:"per_band,omitempty"`
	// PerMode makes each mode class its own slot.
	PerMode bool `toml:"per_mode,omitempty"`
	// Configs are named variants that narrow the bands/modes of this award;
	// each expands to an award of its own with the code "<code>-<name>".
	Configs []Config `toml:"config,omitempty"`
}

// Config is a named configuration of a definition (WAS-20-CW).
type Config struct {
	Name  string   `toml:"name"`
	Bands []string `toml:"bands,omitempty"`
	Modes []string `toml:"modes,omitempty"`
}

// File is a definition file.
type File struct {
	Awards []Definition `toml:"award"`
}

var (
	bandRe = regexp.MustCompile(`^[0-9]+(\.[0-9]+)?(m|cm|mm)$`)
	codeRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.+-]{0,39}$`)
)

// perBand reports whether each band is its own slot.
func (d Definition) perBand() bool { return d.PerBand == nil || *d.PerBand }

// Validate checks a definition (after config expansion).
func (d Definition) Validate() error {
	if !codeRe.MatchString(d.Code) {
		return fmt.Errorf("award code %q: want letters, digits and _ . + -", d.Code)
	}
	switch d.Kind {
	case KindDXCC:
	case KindField:
		if d.Field != "STATE" {
			return fmt.Errorf("award %s: field %q is not supported (only STATE)", d.Code, d.Field)
		}
	case kindReference:
		return fmt.Errorf("award %s: reference awards (SOTA, WWFF, POTA) are deferred and not supported", d.Code)
	case KindContest:
		return fmt.Errorf("award %s: the contest award is built in and cannot be defined in a file", d.Code)
	case "":
		return fmt.Errorf("award %s: kind is required (dxcc or field)", d.Code)
	default:
		return fmt.Errorf("award %s: unknown kind %q (dxcc or field)", d.Code, d.Kind)
	}
	if d.Rule != RuleWorked && d.Rule != RuleConfirmed {
		return fmt.Errorf("award %s: rule %q: want worked or confirmed", d.Code, d.Rule)
	}
	for _, b := range d.Bands {
		if !bandRe.MatchString(b) {
			return fmt.Errorf("award %s: band %q: want a name such as 20m", d.Code, b)
		}
	}
	for _, m := range d.Modes {
		if m != ClassCW && m != ClassPhone && m != ClassData {
			return fmt.Errorf("award %s: mode %q: want cw, phone or data", d.Code, m)
		}
	}
	if d.Kind == KindDXCC && (d.Field != "" || len(d.Items) > 0 || len(d.DXCC) > 0) {
		return fmt.Errorf("award %s: field, items and dxcc belong to kind field", d.Code)
	}
	return nil
}

// normalize fills defaults and canonical forms.
func (d *Definition) normalize() {
	d.Code = strings.TrimSpace(d.Code)
	d.Kind = Kind(strings.ToLower(strings.TrimSpace(string(d.Kind))))
	d.Rule = Rule(strings.ToLower(strings.TrimSpace(string(d.Rule))))
	if d.Rule == "" {
		d.Rule = RuleConfirmed
	}
	if d.Name == "" {
		d.Name = d.Code
	}
	d.Field = strings.ToUpper(strings.TrimSpace(d.Field))
	d.Bands = lowerAll(d.Bands)
	d.Modes = lowerAll(d.Modes)
	for i, it := range d.Items {
		d.Items[i] = strings.ToUpper(strings.TrimSpace(it))
	}
}

func lowerAll(in []string) []string {
	var out []string
	for _, s := range in {
		if s = strings.ToLower(strings.TrimSpace(s)); s != "" {
			out = append(out, s)
		}
	}
	return out
}

// Builtin is the DXCC-per-band award: an entity counts once per band, any
// mode, when a QSO is confirmed (the station log's or Club Log's).
func Builtin() Definition {
	d := Definition{Code: CodeDXCC, Name: "DXCC per band", Kind: KindDXCC, Rule: RuleConfirmed}
	d.normalize()
	return d
}

// Catalog is the set of known awards, by lower-cased code.
type Catalog struct {
	defs  map[string]Definition
	order []string
	// Overrode names the built-in codes a file replaced.
	Overrode []string
}

// NewCatalog returns a catalog holding the built-in DXCC award.
func NewCatalog() *Catalog {
	c := &Catalog{defs: map[string]Definition{}}
	b := Builtin()
	c.defs[strings.ToLower(b.Code)] = b
	c.order = []string{b.Code}
	return c
}

// Add expands, validates and adds a definition. A code from a file replaces
// the built-in DXCC (recorded in Overrode); any other duplicate is an error.
func (c *Catalog) Add(d Definition) error {
	d.normalize()
	all := []Definition{d}
	for _, cfg := range d.Configs {
		n := strings.TrimSpace(cfg.Name)
		if n == "" {
			return fmt.Errorf("award %s: a config needs a name", d.Code)
		}
		v := d
		v.Configs = nil
		switch {
		case strings.EqualFold(n, d.Code):
			continue // Log4OM's config named like the award is the award itself
		case strings.HasPrefix(strings.ToUpper(n), strings.ToUpper(d.Code)+"-"):
			v.Code = n // Log4OM names its configs WAS-10-CW: already prefixed
		default:
			v.Code = d.Code + "-" + n
		}
		v.Name = d.Name + " " + n
		if len(cfg.Bands) > 0 {
			v.Bands = cfg.Bands
		}
		if len(cfg.Modes) > 0 {
			v.Modes = cfg.Modes
		}
		v.normalize()
		all = append(all, v)
	}
	all[0].Configs = nil
	for _, x := range all {
		if err := x.Validate(); err != nil {
			return err
		}
		k := strings.ToLower(x.Code)
		if k == CodeContest {
			return fmt.Errorf("award code %q is reserved for the running contest", x.Code)
		}
		if _, dup := c.defs[k]; dup {
			if k != CodeDXCC || slices.Contains(c.Overrode, CodeDXCC) {
				return fmt.Errorf("award %q is defined twice", x.Code)
			}
			c.Overrode = append(c.Overrode, CodeDXCC)
		} else {
			c.order = append(c.order, x.Code)
		}
		c.defs[k] = x
	}
	return nil
}

// Get returns the award with the code (case-insensitive).
func (c *Catalog) Get(code string) (Definition, bool) {
	d, ok := c.defs[strings.ToLower(strings.TrimSpace(code))]
	return d, ok
}

// Codes lists the award codes in definition order.
func (c *Catalog) Codes() []string { return slices.Clone(c.order) }

// Len is the number of awards.
func (c *Catalog) Len() int { return len(c.defs) }

// LoadDir reads every *.toml file of dir (sorted by name) into a catalog that
// starts with the built-in DXCC. A missing dir, or dir "", gives just the
// built-in award; a bad file is an error naming it.
func LoadDir(dir string) (*Catalog, error) {
	c := NewCatalog()
	if dir == "" {
		return c, nil
	}
	ents, err := os.ReadDir(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return c, nil
	}
	if err != nil {
		return nil, fmt.Errorf("award definitions: %w", err)
	}
	for _, e := range ents {
		if e.IsDir() || !strings.HasSuffix(strings.ToLower(e.Name()), ".toml") {
			continue
		}
		p := filepath.Join(dir, e.Name())
		var f File
		md, err := toml.DecodeFile(p, &f)
		if err != nil {
			return nil, fmt.Errorf("award definitions %s: %w", e.Name(), err)
		}
		if u := md.Undecoded(); len(u) > 0 {
			return nil, fmt.Errorf("award definitions %s: unknown key %s", e.Name(), u[0])
		}
		for _, d := range f.Awards {
			if err := c.Add(d); err != nil {
				return nil, fmt.Errorf("award definitions %s: %w", e.Name(), err)
			}
		}
	}
	return c, nil
}

// ClassOf maps a canonical mode (cw, usb, lsb, am, fm, data) to its mode
// class; "" when the mode is unknown.
func ClassOf(canonical string) string {
	switch canonical {
	case "cw":
		return ClassCW
	case "usb", "lsb", "am", "fm":
		return ClassPhone
	case "data":
		return ClassData
	}
	return ""
}
