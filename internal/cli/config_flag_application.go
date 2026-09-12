package cli

import (
	"flag"
	"reflect"
	"strings"
	"time"
)

// applyConfigFlags applies only visited bindings in declaration order. Providers
// with manual flag policy do not call this engine and keep their ordered checks.
func applyConfigFlags(config, values, report any, fs *flag.FlagSet) error {
	cfg, parsed := reflect.ValueOf(config).Elem(), reflect.ValueOf(values)
	applied := reflect.ValueOf(report).Elem()
	for i := 0; i < cfg.NumField(); i++ {
		field := cfg.Type().Field(i)
		name := field.Tag.Get("flag")
		if name == "" || !flagWasSet(fs, name) {
			continue
		}
		accepted, err := applyConfigFlagField(cfg.Field(i), parsed.FieldByName(field.Name), field.Tag)
		if err != nil {
			return err
		}
		if accepted {
			recordConfigApplied(applied, field)
		}
	}
	return nil
}

func applyConfigFlagField(dst, src reflect.Value, tags reflect.StructTag) (bool, error) {
	if mode := tags.Get("flagDuration"); mode != "" {
		raw := src.Elem().String()
		if mode == "raw-zero-reset" && strings.TrimSpace(raw) == "0s" {
			dst.SetInt(0)
			return true, nil
		}
		if mode == "trim-positive" {
			parsed, err := time.ParseDuration(strings.TrimSpace(raw))
			if err != nil || parsed <= 0 {
				return false, exit(2, "%s", tags.Get("flagDurationError"))
			}
			dst.SetInt(int64(parsed))
			return true, nil
		}
		err := ApplyLeaseDuration(dst.Addr().Interface().(*time.Duration), raw)
		return raw != "" && err == nil, err
	}
	if dst.Kind() == reflect.Slice {
		var value []string
		switch tags.Get("flagList") {
		case "replace-append", "append-trimmed":
			value = append([]string(nil), src.Interface().(flag.Getter).Get().([]string)...)
		case "csv":
			value = splitCSV(src.Elem().String())
		case "empty-scalar", "scalar-empty-nil":
			value = splitCommaList(src.Elem().String())
			if len(value) == 0 {
				value = nil
			}
		default:
			value = splitCommaList(src.Elem().String())
		}
		dst.Set(reflect.ValueOf(value))
	} else if dst.Kind() == reflect.Pointer {
		value := src.Elem().Bool()
		dst.Set(reflect.ValueOf(&value))
	} else {
		dst.Set(src.Elem())
	}
	return true, nil
}

// recordConfigFlagVisits reports raw presence, without applying values or
// inferring accepted input. Manual provider validation consumes this separately.
func recordConfigFlagVisits[Config any](fs *flag.FlagSet, report any) {
	schema, visited := reflect.TypeFor[Config](), reflect.ValueOf(report).Elem()
	for i := 0; i < visited.NumField(); i++ {
		field, _ := schema.FieldByName(visited.Type().Field(i).Name)
		visited.Field(i).SetBool(flagWasSet(fs, field.Tag.Get("flag")))
	}
}
