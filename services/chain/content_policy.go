package chain

import (
	pb "ember/proto"
	"errors"
)

func validateContentPolicy(p *pb.ContentPolicy) error {
	if p == nil {
		return nil
	}
	if p.Id != "prompt-sha256-denylist" || p.Version != "1" || len(p.DeniedPromptSha256) == 0 || len(p.RefusalTokens) == 0 || p.RefusalText == "" {
		return errors.New("unsupported or incomplete content policy")
	}
	seen := map[string]bool{}
	for _, hash := range p.DeniedPromptSha256 {
		if len(hash) != 32 || seen[string(hash)] {
			return errors.New("invalid or duplicate content policy prompt hash")
		}
		seen[string(hash)] = true
	}
	return nil
}
