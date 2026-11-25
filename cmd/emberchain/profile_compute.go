package main

import (
	core "ember/core-go"
	"encoding/json"
	"errors"
	"flag"
	"os"
)

// Expose the same derivation used by publication validation, without a second
// Python implementation of consensus compute or any state mutation.
func deriveCompute(args []string) error {
	f := flag.NewFlagSet("derive-compute", flag.ContinueOnError)
	config := f.String("config", "", "local model config.json")
	shapes := f.String("shapes", "", "JSON tensor shape list from safetensors headers")
	version := f.Uint64("app-version", 16, "target ledger application version")
	if err := f.Parse(args); err != nil {
		return err
	}
	if *config == "" || *shapes == "" {
		return errors.New("--config and --shapes required")
	}
	raw, err := os.ReadFile(*config)
	if err != nil {
		return err
	}
	shapeRaw, err := os.ReadFile(*shapes)
	if err != nil {
		return err
	}
	var tensors []core.TensorShape
	if err = json.Unmarshal(shapeRaw, &tensors); err != nil {
		return err
	}
	result, err := core.DeriveCompute(core.DerivationInput{AppVersion: *version, ConfigJSON: raw, Tensors: tensors})
	if err != nil {
		return err
	}
	return json.NewEncoder(os.Stdout).Encode(result)
}
