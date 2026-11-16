package chain

import (
	pb "ember/proto"
	"google.golang.org/protobuf/proto"
	"testing"
)

func TestContentPolicyManifestValidation(t *testing.T) {
	valid := &pb.ContentPolicy{Id: "prompt-sha256-denylist", Version: "1", DeniedPromptSha256: [][]byte{make([]byte, 32)}, RefusalTokens: []uint32{1}, RefusalText: "Declined"}
	if err := validateContentPolicy(valid); err != nil {
		t.Fatal(err)
	}
	for _, mutate := range []func(*pb.ContentPolicy){
		func(p *pb.ContentPolicy) { p.Id = "" },
		func(p *pb.ContentPolicy) { p.Version = "2" },
		func(p *pb.ContentPolicy) { p.DeniedPromptSha256 = nil },
		func(p *pb.ContentPolicy) { p.DeniedPromptSha256 = [][]byte{{1}} },
		func(p *pb.ContentPolicy) {
			p.DeniedPromptSha256 = append(p.DeniedPromptSha256, p.DeniedPromptSha256[0])
		},
		func(p *pb.ContentPolicy) { p.RefusalTokens = nil },
		func(p *pb.ContentPolicy) { p.RefusalText = "" },
	} {
		p := proto.Clone(valid).(*pb.ContentPolicy)
		mutate(p)
		if err := validateRangeRules(&pb.ModelManifest{ContentPolicy: p}); err == nil {
			t.Fatal("invalid manifest policy accepted", p)
		}
	}
	if err := validateRangeRules(&pb.ModelManifest{ContentPolicy: valid, DynamicRanges: true}); err == nil {
		t.Fatal("unsupported pipeline policy accepted")
	}
}
