package main

import (
	"bytes"
	"fmt"
	"os"

	jsonschema "github.com/steipete/jsonschema/v6"
)

func main() {
	if len(os.Args) != 3 {
		fmt.Fprintln(os.Stderr, "usage: go run ./scripts/validate-json-schema SCHEMA DOCUMENT")
		os.Exit(2)
	}

	schemaBytes, err := os.ReadFile(os.Args[1])
	if err != nil {
		fatalf("read schema: %v", err)
	}
	schemaDocument, err := jsonschema.UnmarshalJSON(bytes.NewReader(schemaBytes))
	if err != nil {
		fatalf("parse schema: %v", err)
	}
	compiler := jsonschema.NewCompiler()
	compiler.DefaultDraft(jsonschema.Draft2020)
	const schemaURL = "https://crabbox.invalid/qualification-input.schema.json"
	if err := compiler.AddResource(schemaURL, schemaDocument); err != nil {
		fatalf("load schema: %v", err)
	}
	compiled, err := compiler.Compile(schemaURL)
	if err != nil {
		fatalf("compile schema: %v", err)
	}

	documentBytes, err := os.ReadFile(os.Args[2])
	if err != nil {
		fatalf("read document: %v", err)
	}
	document, err := jsonschema.UnmarshalJSON(bytes.NewReader(documentBytes))
	if err != nil {
		fatalf("parse document: %v", err)
	}
	if err := compiled.Validate(document); err != nil {
		fatalf("validate document: %v", err)
	}
}

func fatalf(format string, args ...any) {
	fmt.Fprintf(os.Stderr, format+"\n", args...)
	os.Exit(1)
}
