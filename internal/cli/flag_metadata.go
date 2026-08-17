package cli

import (
	"flag"
	"sync"
)

type registeredFlagAnnotation struct {
	Deprecated  bool
	Replacement string
}

var registeredFlagAnnotations sync.Map

// MarkFlagDeprecated attaches deprecation metadata to a flag's real registered
// value. Missing flags or replacements panic during registration so provider
// authoring drift cannot silently reach discovery output.
func MarkFlagDeprecated(fs *flag.FlagSet, name, replacement string) {
	item := fs.Lookup(name)
	if item == nil {
		panic("cannot deprecate unregistered flag --" + name)
	}
	if replacement == "" || replacement == name || fs.Lookup(replacement) == nil {
		panic("deprecated flag --" + name + " has invalid replacement --" + replacement)
	}
	if _, ok := registeredFlagAnnotations.Load(item); ok {
		panic("flag --" + name + " already has metadata")
	}
	registeredFlagAnnotations.Store(item, registeredFlagAnnotation{
		Deprecated:  true,
		Replacement: replacement,
	})
}

func annotationForFlag(item *flag.Flag) registeredFlagAnnotation {
	if annotation, ok := registeredFlagAnnotations.Load(item); ok {
		return annotation.(registeredFlagAnnotation)
	}
	return registeredFlagAnnotation{}
}

func clearFlagAnnotations(fs *flag.FlagSet) {
	fs.VisitAll(func(item *flag.Flag) { registeredFlagAnnotations.Delete(item) })
}
