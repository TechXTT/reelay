package config

import (
	"fmt"
	"os"
	"reflect"
	"strconv"
	"strings"
)

const envPrefix = "REELAY_"

// EnvKey renders the environment variable name for a dotted config path.
//
//	server.auth_token -> REELAY_SERVER_AUTH_TOKEN
func EnvKey(dotted string) string {
	return envPrefix + strings.ToUpper(strings.ReplaceAll(dotted, ".", "_"))
}

// applyEnv walks the config struct and overrides any scalar (or []string)
// field whose REELAY_* variable is set.
//
// Slices of structs — indexers, profiles, path_mappings — are deliberately not
// reachable: there is no sane env encoding for "the third indexer's base URL",
// and inventing one produces config that nobody can read back.
func applyEnv(c *Config) error {
	return walkEnv(reflect.ValueOf(c).Elem(), "", os.LookupEnv)
}

type lookupFunc func(key string) (string, bool)

func walkEnv(v reflect.Value, prefix string, look lookupFunc) error {
	t := v.Type()
	for i := 0; i < t.NumField(); i++ {
		sf := t.Field(i)
		tag := sf.Tag.Get("yaml")
		if tag == "" || tag == "-" {
			continue
		}
		name := strings.Split(tag, ",")[0]
		path := name
		if prefix != "" {
			path = prefix + "." + name
		}
		fv := v.Field(i)

		// Duration is a struct but behaves as a scalar.
		if fv.Type() == reflect.TypeOf(Duration{}) {
			if raw, ok := look(EnvKey(path)); ok {
				d := Duration{}
				if err := d.Set(raw); err != nil {
					return fmt.Errorf("%s: %w", EnvKey(path), err)
				}
				fv.Set(reflect.ValueOf(d))
			}
			continue
		}

		switch fv.Kind() {
		case reflect.Struct:
			if err := walkEnv(fv, path, look); err != nil {
				return err
			}
		case reflect.Slice:
			// Only []string is overridable, comma separated.
			if fv.Type().Elem().Kind() != reflect.String {
				continue
			}
			if raw, ok := look(EnvKey(path)); ok {
				fv.Set(reflect.ValueOf(splitList(raw)))
			}
		case reflect.String:
			if raw, ok := look(EnvKey(path)); ok {
				fv.SetString(raw)
			}
		case reflect.Bool:
			if raw, ok := look(EnvKey(path)); ok {
				b, err := strconv.ParseBool(raw)
				if err != nil {
					return fmt.Errorf("%s: invalid boolean %q (want true/false)", EnvKey(path), raw)
				}
				fv.SetBool(b)
			}
		case reflect.Int, reflect.Int64:
			if raw, ok := look(EnvKey(path)); ok {
				n, err := strconv.ParseInt(raw, 10, 64)
				if err != nil {
					return fmt.Errorf("%s: invalid integer %q", EnvKey(path), raw)
				}
				fv.SetInt(n)
			}
		case reflect.Float64:
			if raw, ok := look(EnvKey(path)); ok {
				f, err := strconv.ParseFloat(raw, 64)
				if err != nil {
					return fmt.Errorf("%s: invalid number %q", EnvKey(path), raw)
				}
				fv.SetFloat(f)
			}
		}
	}
	return nil
}

func splitList(raw string) []string {
	if strings.TrimSpace(raw) == "" {
		return []string{}
	}
	parts := strings.Split(raw, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}
