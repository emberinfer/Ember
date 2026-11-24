package main

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	core "ember/core-go"
	pb "ember/proto"
	"ember/services/chain"
	"ember/services/ledger"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"google.golang.org/protobuf/proto"
	"io"
	"os"
	"sort"
)

const base = "/root/autodl-tmp/t2i/"

func must(e error) {
	if e != nil {
		panic(e)
	}
}
func write(name string, v any) {
	b, e := json.MarshalIndent(v, "", "  ")
	must(e)
	must(os.WriteFile(base+"work2/chain-genesis/ember-sd15-fp32-v2-republish/"+name, b, 0600))
}
func path(n string) string {
	if n == "scheduler_cfg.json" || n == "arch_manifest.json" {
		return base + "work2/chain-genesis/ember-sd15-fp32-v2-republish/" + n
	}
	return base + "sd15/" + n
}
func main() {
	raw, e := os.ReadFile(base + "work2/chain-genesis/ember-sd15-fp32-v2-republish/image-profile.json")
	must(e)
	var p core.ImageProfile
	must(json.Unmarshal(raw, &p))
	readers := map[string]io.Reader{}
	sizes := map[string]int64{}
	for _, c := range p.Components {
		for n := range c.Files {
			f, e := os.Open(path(n))
			must(e)
			defer f.Close()
			st, e := f.Stat()
			must(e)
			sizes[n] = st.Size()
			readers[n] = f
		}
	}
	headers, e := ledger.VerifyImageMaterials(&p, readers)
	must(e)
	fmt.Println("REAL_LEDGER_OK all component artifacts streamed; measured band")
	_, key, e := ed25519.GenerateKey(rand.Reader)
	must(e)
	for _, tc := range []string{"good"} {
		var trial core.ImageProfile
		must(json.Unmarshal(raw, &trial))
		switch tc {
		case "plus5":
			trial.PriceTable[0].Step = trial.PriceTable[0].Step * 105 / 100
		case "minus5":
			trial.PriceTable[0].Step = trial.PriceTable[0].Step * 95 / 100
		case "channel":
			trial.ArchManifest["unet"][0].Shape[0]++
		}
		model := "ember-stable-diffusion-1.5-v2"
		pr, _ := json.Marshal(&trial)
		gh := sha256.Sum256(pr)
		cfg, e := os.ReadFile(base + "sd15/unet/config.json")
		must(e)
		ch := sha256.Sum256(cfg)
		tok, _ := hex.DecodeString(trial.Components[3].Files["tokenizer/vocab.json"])
		m := &pb.ModelManifest{ChainId: model, ConfigHash: ch[:], GraphHash: gh[:], Artifacts: []*pb.Artifact{{Name: "config.json", Sha256: ch[:], Size: int64(len(cfg)), Source: "git:config.json"}}}
		for _, c := range trial.Components {
			names := []string{}
			for n := range c.Files {
				names = append(names, n)
			}
			sort.Strings(names)
			for _, n := range names {
				h, _ := hex.DecodeString(c.Files[n])
				m.Artifacts = append(m.Artifacts, &pb.Artifact{Name: n, Sha256: h, Size: sizes[n], Source: "git:" + n})
			}
		}
		for _, name := range []string{"vae/config.json", "text_encoder/config.json"} {
			data, e := os.ReadFile(base + "sd15/" + name)
			must(e)
			h := sha256.Sum256(data)
			pinsRaw, e := os.ReadFile(base + "work2/node/engine/assets/sd15-fp32-v1.json")
			must(e)
			var pins struct {
				Files map[string]string `json:"files"`
			}
			must(json.Unmarshal(pinsRaw, &pins))
			if hex.EncodeToString(h[:]) != pins.Files[name] {
				panic("asset pin mismatch: " + name)
			}
			m.Artifacts = append(m.Artifacts, &pb.Artifact{Name: name, Sha256: h[:], Size: int64(len(data)), Source: "git:" + name})
		}
		if len(m.Artifacts) != 12 {
			panic("expected twelve artifacts")
		}
		must(core.Sign(m, key))
		root, e := core.Digest(m)
		must(e)
		g := &pb.ChainGenesis{ChainId: model, ModelManifestRoot: root, TokenizerHash: tok, ExecutionProfileVer: trial.ExecutionProfile, PriceOutput: 1}
		must(core.Sign(g, key))
		gr, e := proto.Marshal(g)
		must(e)
		mr, e := proto.Marshal(m)
		must(e)
		must(os.WriteFile(base+"work2/chain-genesis/ember-sd15-fp32-v2-republish/genesis.pb", gr, 0600))
		must(os.WriteFile(base+"work2/chain-genesis/ember-sd15-fp32-v2-republish/manifest.pb", mr, 0600))
		genHash := sha256.Sum256(gr)
		profHash, e := core.Digest(g)
		must(e)
		action := chain.Action{Type: "register_model", Kind: "text-to-image", ModelID: model, GenesisHash: hex.EncodeToString(genHash[:]), ProfileHash: hex.EncodeToString(profHash), Genesis: gr, Manifest: mr, ImageProfile: &trial, ImageHeaders: headers, Origin: &chain.Origin{Repository: "https://huggingface.co/stable-diffusion-v1-5/stable-diffusion-v1-5", Revision: "451f4fe16113bff5a5d2269ed5ad43b0592e9a14"}}
		write("registration-template.json", action)
	}
}
