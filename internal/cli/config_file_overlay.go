package cli

import (
	"reflect"
	"time"
)

// applyConfigFileOverlay overlays the generated YAML DTO using the validated schema's
// source grants and value policy. Provider admission wrappers still prepare the
// DTO before this call; the original persisted file is never modified here.
func applyConfigFileOverlay(config, input, report any, trusted bool, provider string) error {
	file := reflect.ValueOf(input)
	if file.IsNil() {
		return nil
	}
	file = file.Elem()
	cfg, applied := reflect.ValueOf(config).Elem(), reflect.ValueOf(report).Elem()
	for i := 0; i < cfg.NumField(); i++ {
		field := cfg.Type().Field(i)
		switch field.Tag.Get("sources") {
		case "user,repo,env,flag", "user,repo,env":
		case "user,env,flag", "user,env":
			if !trusted {
				continue
			}
		default:
			continue
		}
		members := []string{field.Name}
		if field.Tag.Get("configAlias") != "" {
			members = append(members, field.Name+"ConfigAlias")
		}
		for _, member := range members {
			accepted, err := applyConfigFileField(cfg.Field(i), file.FieldByName(member), field.Tag, provider)
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
	}
	return nil
}

func applyConfigFileField(dst, src reflect.Value, tags reflect.StructTag, provider string) (bool, error) {
	if src.Kind() == reflect.Pointer {
		if src.IsNil() {
			return false, nil
		}
		src = src.Elem()
	}
	if dst.Type() == reflect.TypeFor[time.Duration]() {
		return applyLeaseDuration(dst.Addr().Interface().(*time.Duration), src.String()), nil
	}
	switch dst.Kind() {
	case reflect.String:
		if tags.Get("fileIgnoreEmpty") == "true" && src.String() == "" {
			return false, nil
		}
	case reflect.Int, reflect.Int64:
		value := src.Int()
		switch tags.Get("fileInt") {
		case "positive":
			if value <= 0 {
				return false, nil
			}
		case "nonzero":
			if value == 0 {
				return false, nil
			}
		case "present":
		default:
			if value < 0 {
				return false, exit(2, "%s %s must be non-negative", provider, tags.Get("config"))
			}
		}
	case reflect.Float64:
		if tags.Get("fileFloat") == "positive" && !(src.Float() > 0) {
			return false, nil
		}
	case reflect.Slice:
		value := src.Interface().([]string)
		switch tags.Get("fileList") {
		case "raw":
			if tags.Get("fileStorage") == "value" && value == nil {
				return false, nil
			}
			value = append([]string(nil), value...)
		case "nonempty-raw":
			if len(value) == 0 {
				return false, nil
			}
		case "nonempty-normalized":
			if len(value) == 0 {
				return false, nil
			}
			value = normalizeList(value)
		default:
			value = normalizeList(value)
		}
		src = reflect.ValueOf(value)
	case reflect.Pointer:
		value := src.Bool()
		src = reflect.ValueOf(&value)
	}
	dst.Set(src)
	return true, nil
}
