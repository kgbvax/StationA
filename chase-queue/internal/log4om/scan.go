package log4om

import "fmt"

func fmtSscan(s string, f *float64) (int, error) { return fmt.Sscan(s, f) }
