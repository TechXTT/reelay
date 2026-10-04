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

var durationType = reflect.TypeOf(Duration{})

func walkEnv(v reflect.Value, prefix string, look lookupFunc) error {
	t := v.Type()
	for i := 0; i < t.NumField(); i++ {
		tag := t.Field(i).Tag.Get("yaml")
		if tag == "" || tag == "-" {
			continue
		}
		path := strings.Split(tag, ",")[0]
		if prefix != "" {
			path = prefix + "." + path
		}
		field := v.Field(i)
		// Duration is a struct but behaves as a scalar.
		if field.Kind() == reflect.Struct && field.Type() != durationType {
			if err := walkEnv(field, path, look); err != nil {
				return err
			}
			continue
		}
		key := EnvKey(path)
		raw, ok := look(key)
		if !ok {
			continue
		}
		if err := setFromEnv(field, key, raw); err != nil {
			return err
		}
	}
	return nil
}

func setFromEnv(field reflect.Value, key, raw string) error {
	if field.Type() == durationType {
		var d Duration
		if err := d.Set(raw); err != nil {
			return fmt.Errorf("%s: %w", key, err)
		}
		field.Set(reflect.ValueOf(d))
		return nil
	}
	switch field.Kind() {
	case reflect.Slice:
		// Struct slices are intentionally not configurable through the environment.
		if field.Type().Elem().Kind() == reflect.String {
			field.Set(reflect.ValueOf(splitList(raw)))
		}
	case reflect.String:
		field.SetString(raw)
	case reflect.Bool:
		b, err := strconv.ParseBool(raw)
		if err != nil {
			return fmt.Errorf("%s: invalid boolean %q (want true/false)", key, raw)
		}
		field.SetBool(b)
	case reflect.Int, reflect.Int64:
		n, err := strconv.ParseInt(raw, 10, 64)
		if err != nil {
			return fmt.Errorf("%s: invalid integer %q", key, raw)
		}
		field.SetInt(n)
	case reflect.Float64:
		f, err := strconv.ParseFloat(raw, 64)
		if err != nil {
			return fmt.Errorf("%s: invalid number %q", key, raw)
		}
		field.SetFloat(f)
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
