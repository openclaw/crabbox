package cli

import "reflect"

// nonNullJSONCollection keeps CLI inventories empty rather than null. Only the
// outer collection changes; nullable fields and non-empty values stay intact.
// An untyped nil is an empty list, as returned by JSONListBackend implementations.
func nonNullJSONCollection(value any) any {
	if value == nil {
		return []any{}
	}
	v := reflect.ValueOf(value)
	switch v.Kind() {
	case reflect.Slice:
		if v.IsNil() {
			return reflect.MakeSlice(v.Type(), 0, 0).Interface()
		}
	case reflect.Map:
		if v.IsNil() {
			return reflect.MakeMap(v.Type()).Interface()
		}
	}
	return value
}
