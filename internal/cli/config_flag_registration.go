package cli

import (
	"flag"
	"reflect"
	"strings"
	"time"
)

// registerConfigFlags populates generated, typed flag storage from a validated
// schema. List constructors own their snapshots; registration never applies a
// value to the runtime config or records input provenance.
func registerConfigFlags(fs *flag.FlagSet, defaults, values any) {
	cfg, parsed := reflect.ValueOf(defaults), reflect.ValueOf(values).Elem()
	// Keep the existing order: replacing lists, ordinary flags, appending lists.
	for phase := 0; phase < 3; phase++ {
		for i := 0; i < parsed.NumField(); i++ {
			name := parsed.Type().Field(i).Name
			field, _ := cfg.Type().FieldByName(name)
			fieldPhase := 1
			switch field.Tag.Get("flagList") {
			case "replace-append":
				fieldPhase = 0
			case "append-trimmed":
				fieldPhase = 2
			}
			if fieldPhase == phase {
				parsed.Field(i).Set(reflect.ValueOf(registerConfigFlag(fs, cfg.FieldByName(name), field.Tag)))
			}
		}
	}
}

func registerConfigFlag(fs *flag.FlagSet, value reflect.Value, tags reflect.StructTag) any {
	name, help := tags.Get("flag"), tags.Get("help")
	if value.Type() == reflect.TypeFor[time.Duration]() {
		duration := value.Interface().(time.Duration)
		if tags.Get("flagDuration") != "" {
			return fs.String(name, duration.String(), help)
		}
		return fs.Duration(name, duration, help)
	}
	switch value.Kind() {
	case reflect.String:
		text := value.String()
		if fallback := tags.Get("flagFallback"); fallback != "" {
			text = blank(text, fallback)
		}
		return fs.String(name, text, help)
	case reflect.Int:
		return fs.Int(name, int(value.Int()), help)
	case reflect.Int64:
		return fs.Int64(name, value.Int(), help)
	case reflect.Float64:
		return fs.Float64(name, value.Float(), help)
	case reflect.Bool:
		return fs.Bool(name, value.Bool(), help)
	case reflect.Pointer:
		return fs.Bool(name, !value.IsNil() && value.Elem().Bool(), help)
	case reflect.Slice:
		defaults := value.Interface().([]string)
		switch tags.Get("flagList") {
		case "replace-append":
			list := newReplaceAppendListFlag(defaults)
			fs.Var(list, name, help)
			return list
		case "append-trimmed":
			list := newAppendTrimmedListFlag(defaults)
			fs.Var(list, name, help)
			return list
		case "empty-scalar":
			return fs.String(name, "", help)
		default:
			return fs.String(name, strings.Join(defaults, ","), help)
		}
	default:
		panic("configgen admitted an unsupported flag type")
	}
}
