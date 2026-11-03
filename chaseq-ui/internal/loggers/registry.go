package loggers

import (
	"fmt"
	"sort"
	"strings"
	"time"
)

// Spec is one configured sender, as the config file gives it.
type Spec struct {
	Kind    string
	Addr    string
	Timeout time.Duration // 0 = the kind's default
}

// constructors maps a kind to its Sender constructor. A new logger is one
// entry here plus its Sender.
var constructors = map[string]func(Spec) (Sender, error){
	"log4om": func(s Spec) (Sender, error) {
		if s.Addr == "" {
			return nil, fmt.Errorf("log4om needs addr (host:port of the Remote Control inbound UDP)")
		}
		return NewLog4OM(s.Addr, s.Timeout), nil
	},
}

// Kinds lists the known sender kinds, sorted.
func Kinds() []string {
	var k []string
	for name := range constructors {
		k = append(k, name)
	}
	sort.Strings(k)
	return k
}

// Build constructs one Sender per spec, in order. An unknown kind or a bad
// entry is an error naming the entry, so a typo in config.toml fails at start.
func Build(specs []Spec) ([]Sender, error) {
	var out []Sender
	for i, sp := range specs {
		kind := strings.ToLower(strings.TrimSpace(sp.Kind))
		mk, ok := constructors[kind]
		if !ok {
			return nil, fmt.Errorf("loggers.sender #%d: unknown kind %q (known: %s)", i+1, sp.Kind, strings.Join(Kinds(), ", "))
		}
		s, err := mk(sp)
		if err != nil {
			return nil, fmt.Errorf("loggers.sender #%d (%s): %w", i+1, kind, err)
		}
		out = append(out, s)
	}
	return out, nil
}
