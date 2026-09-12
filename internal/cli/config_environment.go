package cli

import (
	"os"
	"reflect"
	"strconv"
	"strings"
	"time"
)

// applyConfigEnvironment executes the environment policy in a configgen-validated
// schema. Field order and split boundaries preserve partial application and the
// handwritten normalization that some providers perform between two passes.
func applyConfigEnvironment(config, report any, start, end int) error {
	cfg, applied := reflect.ValueOf(config).Elem(), reflect.ValueOf(report).Elem()
	position := 0
	for i := 0; i < cfg.NumField(); i++ {
		field := cfg.Type().Field(i)
		if field.Tag.Get("sources") == "runtime" {
			continue
		}
		selected := position >= start && position < end
		position++
		if !selected {
			continue
		}
		if field.Tag.Get("sources") == "flag" {
			continue
		}
		accepted, err := applyConfigEnvironmentField(cfg.Field(i), field.Tag)
		if err != nil {
			return err
		}
		if accepted {
			applied.FieldByName("InputAccepted").SetBool(true)
			if field.Tag.Get("reportApplied") == "true" {
				applied.FieldByName(field.Name).SetBool(true)
			}
		}
	}
	return nil
}

func applyConfigEnvironmentField(dst reflect.Value, tags reflect.StructTag) (bool, error) {
	name, alias := tags.Get("env"), tags.Get("envAlias")
	if dst.Type() == reflect.TypeFor[time.Duration]() {
		return applyLeaseDuration(dst.Addr().Interface().(*time.Duration), os.Getenv(name)), nil
	}
	switch dst.Kind() {
	case reflect.String:
		names := []string{name}
		if alias != "" && (tags.Get("envAliasAfterConfig") != "true" || dst.String() == "") {
			names = append(names, alias)
		}
		if second := tags.Get("envAlias2"); second != "" {
			names = append(names, second)
		}
		value, accepted := firstNonEmptyEnv(names...)
		if accepted {
			dst.SetString(value)
		}
		return accepted, nil
	case reflect.Int, reflect.Int64:
		if tags.Get("envInt") == "fallback" {
			bits := 64
			if dst.Kind() == reflect.Int {
				bits = strconv.IntSize
			}
			var value int64
			var accepted bool
			if alias != "" {
				value, accepted = lookupEnvInteger(alias, bits)
			}
			if primary, ok := lookupEnvInteger(name, bits); ok {
				value, accepted = primary, true
			}
			if accepted {
				dst.SetInt(value)
			}
			return accepted, nil
		}
		if tags.Get("envInt") == "checked-alias" {
			value, accepted, err := getenvNonNegativeIntAliasAccepted(name, alias, int(dst.Int()))
			if err == nil && accepted {
				dst.SetInt(int64(value))
			}
			return accepted, err
		}
		value, accepted, err := getenvNonNegativeIntAccepted(name, int(dst.Int()))
		// Strict scalar input historically clears this field before returning an
		// error; checked-alias input above leaves its previous value intact.
		dst.SetInt(int64(value))
		return accepted, err
	case reflect.Float64:
		value, accepted := lookupEnvFloat(name)
		if accepted {
			dst.SetFloat(value)
		}
		return accepted, nil
	case reflect.Bool, reflect.Pointer:
		value, accepted := getenvBool(name)
		if !accepted && alias != "" {
			value, accepted = getenvBool(alias)
		}
		if accepted {
			if dst.Kind() == reflect.Pointer {
				dst.Set(reflect.ValueOf(&value))
			} else {
				dst.SetBool(value)
			}
		}
		return accepted, nil
	case reflect.Slice:
		raw := os.Getenv(name)
		var value []string
		accepted := raw != ""
		switch tags.Get("envList") {
		case "presence":
			value, accepted = getenvList(name)
		case "trimmed-nonempty":
			accepted = strings.TrimSpace(raw) != ""
			value = parseEnvListValue(raw)
		case "csv":
			value = splitCSV(raw)
		default:
			value = splitCommaList(raw)
		}
		if accepted {
			dst.Set(reflect.ValueOf(value))
		}
		return accepted, nil
	default:
		panic("configgen admitted an unsupported environment field type")
	}
}
