package chain

import "errors"

// ExecExitDelayBlocks makes exit effective at submission height + 1000 blocks;
// at ~1.5–1.9 s/block this is a ~25–32 min observable pending window.
const ExecExitDelayBlocks = 1000

func (s *State) requestExecExit(signer string, height int64) error {
	if s.Version < 13 {
		return errors.New("execution exit upgrade not active")
	}
	r := s.Nodes[signer]
	if r == nil || r.ExpiresHeight <= height {
		return errors.New("execution exit requires active owner registration")
	}
	if r.NoExec {
		return errors.New("registration already opted out of execution")
	}
	if r.PendingNoExec {
		if height >= r.ExecExitEffectiveHeight {
			return errors.New("execution exit already effective")
		}
		return errors.New("execution exit already pending")
	}
	r.PendingNoExec = true
	r.ExecExitEffectiveHeight = height + ExecExitDelayBlocks
	return nil
}
