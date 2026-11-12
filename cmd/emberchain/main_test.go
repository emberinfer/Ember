package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"ember/services/chain"
)

func TestPublicGenesisCeremony(t *testing.T) {
	root := t.TempDir()
	homes := []string{filepath.Join(root, "v1"), filepath.Join(root, "v2"), filepath.Join(root, "v3")}
	descriptors := make([]string, len(homes))
	for i, home := range homes {
		if err := initValidator([]string{"--home", home}); err != nil {
			t.Fatal(err)
		}
		descriptors[i] = filepath.Join(root, "v"+string(rune('1'+i))+".json")
		if err := validatorPublic([]string{"--home", home, "--name", "operator-" + string(rune('a'+i)), "--out", descriptors[i]}); err != nil {
			t.Fatal(err)
		}
	}
	app := chain.GenesisState{Parameters: chain.Parameters{ChainID: "ember-ceremony-test", SubsidyCap: 0,
		UnbondingBlocks: 1, SlashMultiplier: 20, MinNodeStake: 1, HoldTimeout: 2, RegistrationTTL: 2}}
	appRaw, err := json.Marshal(app)
	if err != nil {
		t.Fatal(err)
	}
	appPath := filepath.Join(root, "app-state.json")
	if err = os.WriteFile(appPath, appRaw, 0600); err != nil {
		t.Fatal(err)
	}
	genesisPath := filepath.Join(root, "genesis.json")
	publicSet := strings.Join(descriptors, ",")
	if err = genesis([]string{"--out", genesisPath, "--chain-id", "ember-ceremony-test", "--app-state", appPath, "--validator-public", publicSet}); err != nil {
		t.Fatal(err)
	}
	for _, home := range homes {
		if err = verifyGenesis([]string{"--genesis", genesisPath, "--app-state", appPath, "--validator-public", publicSet, "--local-home", home}); err != nil {
			t.Fatal(err)
		}
	}
}

func TestDescriptorRejectsAddressSubstitution(t *testing.T) {
	root := t.TempDir()
	home := filepath.Join(root, "validator")
	if err := initValidator([]string{"--home", home}); err != nil {
		t.Fatal(err)
	}
	descriptor, err := descriptorFromHome(home, "operator")
	if err != nil {
		t.Fatal(err)
	}
	descriptor.ValidatorAddress = strings.Repeat("0", len(descriptor.ValidatorAddress))
	if _, err = descriptorGenesisValidator(descriptor); err == nil {
		t.Fatal("substituted validator address accepted")
	}
}
