package cli

import (
	"fmt"
	"os"
	"strings"
)

// flags is a small parser that accepts flags anywhere on the line, so
// "cubby set path value --create" works as well as the other order.
type flags struct {
	vals map[string]string
	set  map[string]bool
	args []string
}

func parseFlags(in []string, withValue, boolean []string) (*flags, error) {
	f := &flags{vals: map[string]string{}, set: map[string]bool{}}
	isVal := map[string]bool{}
	for _, n := range withValue {
		isVal[n] = true
	}
	isBool := map[string]bool{}
	for _, n := range boolean {
		isBool[n] = true
	}
	for i := 0; i < len(in); i++ {
		a := in[i]
		if a == "--" {
			f.args = append(f.args, in[i+1:]...)
			break
		}
		// Values like -5 or -0.5 are arguments, not flags.
		if len(a) < 2 || a[0] != '-' || (a[1] >= '0' && a[1] <= '9') || a[1] == '.' {
			f.args = append(f.args, a)
			continue
		}
		name := strings.TrimLeft(a, "-")
		val, hasVal := "", false
		if k, v, ok := strings.Cut(name, "="); ok {
			name, val, hasVal = k, v, true
		}
		switch {
		case isVal[name]:
			if !hasVal {
				if i+1 >= len(in) {
					return nil, fmt.Errorf("flag --%s needs a value", name)
				}
				i++
				val = in[i]
			}
			f.vals[name] = val
			f.set[name] = true
		case isBool[name]:
			if hasVal {
				return nil, fmt.Errorf("flag --%s takes no value", name)
			}
			f.set[name] = true
		default:
			return nil, fmt.Errorf("unknown flag %s", a)
		}
	}
	return f, nil
}

func (f *flags) str(n string) string { return f.vals[n] }

func (f *flags) bool(n string) bool { return f.set[n] }

func (f *flags) has(n string) bool { return f.set[n] }

func usageErr(err error) int {
	fmt.Fprintf(os.Stderr, "cubby: %v\nRun 'cubby help' for usage.\n", err)
	return 64
}

func firstNonEmpty(v ...string) string {
	for _, s := range v {
		if s != "" {
			return s
		}
	}
	return ""
}
