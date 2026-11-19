package chain

import (
	"encoding/hex"
	"encoding/json"
	"errors"
	"math"

	core "ember/core-go"
	pb "ember/proto"
	"google.golang.org/protobuf/proto"
)

// ThresholdPair is immutable evidence-backed policy for one model/band/length.
// Placeholder explicitly prevents treating short-corpus defaults as calibration.
type ThresholdPair struct {
	Model          string    `json:"model"`
	Band           string    `json:"band"`
	LengthClass    string    `json:"length_class"`
	MinOutput      uint32    `json:"min_output"`
	MaxOutput      uint32    `json:"max_output"`
	Soft           float64   `json:"theta_soft"`
	Hard           float64   `json:"theta_hard"`
	HonestScores   []float64 `json:"honest_scores"`
	CalibrationRef string    `json:"calibration_ref"`
	Placeholder    bool      `json:"placeholder"`
}

type MisjudgeParameters struct {
	ConsumerGray    ConsumerGrayParameters `json:"consumer_gray"`
	SlashMode       string                 `json:"slash_mode"`
	GrayRepeatLimit uint32                 `json:"gray_repeat_limit"`
	WindowBlocks    int64                  `json:"window_blocks"`
	CapBlockBPS     uint32                 `json:"cap_block_bps"`
	Thresholds      []ThresholdPair        `json:"thresholds,omitempty"`
}

func (p *MisjudgeParameters) defaults() {
	p.ConsumerGray.defaults()
	if p.SlashMode == "" {
		p.SlashMode = "gray_gate"
	}
	if p.GrayRepeatLimit == 0 {
		p.GrayRepeatLimit = 3
	}
	if p.WindowBlocks == 0 {
		p.WindowBlocks = 1000
	}
	if p.CapBlockBPS == 0 {
		p.CapBlockBPS = 500
	}
}
func (p MisjudgeParameters) valid() bool {
	if p.SlashMode != "off" && p.SlashMode != "refund_only" && p.SlashMode != "gray_gate" && p.SlashMode != "full" {
		return false
	}
	if !p.ConsumerGray.valid() || p.GrayRepeatLimit == 0 || p.WindowBlocks < 1 || p.CapBlockBPS == 0 || p.CapBlockBPS > 10000 {
		return false
	}
	for i, t := range p.Thresholds {
		if t.Model == "" || t.Band == "" || t.LengthClass == "" || t.MinOutput > t.MaxOutput || !finite(t.Soft) || !finite(t.Hard) || t.Soft < 0 || t.Soft >= t.Hard || len(t.HonestScores) == 0 || len(t.CalibrationRef) != 64 {
			return false
		}
		if _, err := hex.DecodeString(t.CalibrationRef); err != nil {
			return false
		}
		for _, score := range t.HonestScores {
			if !finite(score) || score < 0 {
				return false
			}
		}
		for _, prior := range p.Thresholds[:i] {
			if prior.Model == t.Model && prior.Band == t.Band && (prior.LengthClass == t.LengthClass || prior.MinOutput <= t.MaxOutput && t.MinOutput <= prior.MaxOutput) {
				return false
			}
		}
	}
	return true
}
func finite(v float64) bool { return !math.IsNaN(v) && !math.IsInf(v, 0) }

// OffenseRecord v2 is distinct from the bounded slash-ladder height history.
type OffenseRecord struct {
	Version     uint32         `json:"version"`
	HoldID      string         `json:"hold_id"`
	NodeID      string         `json:"node_id"`
	Height      int64          `json:"height"`
	Class       string         `json:"class"`
	Score       float64        `json:"score"`
	Band        string         `json:"band"`
	LengthClass string         `json:"length_class"`
	ThetaPair   *ThresholdPair `json:"theta_pair"`
	Fault       string         `json:"fault,omitempty"`
	Status      string         `json:"status"`
}

type BlockSlash struct {
	Height     int64  `json:"height"`
	StakeStart int64  `json:"stake_start"`
	Paid       int64  `json:"paid"`
	Limit      int64  `json:"limit"`
	Multiplier uint32 `json:"multiplier"`
}

func (s *State) validateGrade(h *Hold, raw []byte) (*pb.OffenseGrade, error) {
	v := &pb.AuditVerdict{}
	if proto.Unmarshal(raw, v) != nil || v.Offense == nil {
		return nil, errors.New("rejection requires signed offense grading")
	}
	g := v.Offense
	if !finite(g.Score) || g.Score < 0 {
		return nil, errors.New("invalid offense score")
	}
	switch g.Class {
	case "objective":
		if g.Score != 0 || g.Band != "" || g.LengthClass != "" {
			return nil, errors.New("objective offense cannot carry numerical grading")
		}
		switch g.Fault {
		case "n_in":
			c := &pb.ExecutionCommitment{}
			if h.TokenFee == nil || proto.Unmarshal(h.TokenFee.Commitment, c) != nil || c.TokenizerVersion == "" || g.TokenizerVersion != c.TokenizerVersion || g.RecomputedInputTokens == 0 || g.RecomputedInputTokens == c.InputTokens {
				return nil, errors.New("N_in offense requires mismatched count with frozen tokenizer version")
			}
		case "signature", "hash", "format":
			if g.TokenizerVersion != "" || g.RecomputedInputTokens != 0 {
				return nil, errors.New("unexpected tokenizer evidence")
			}
		default:
			return nil, errors.New("unknown objective fault")
		}
	case "token":
		if g.Band != "token-equality-v1" || g.Fault != "" || g.LengthClass != "" {
			return nil, errors.New("invalid token offense")
		}
	case "numerical":
		if g.Fault != "" || g.TokenizerVersion != "" || g.RecomputedInputTokens != 0 {
			return nil, errors.New("numerical offense cannot claim objective fault")
		}
		if h.Pipeline != nil {
			if s.dynamicPipeline(h) {
				if g.Band != core.RangeBand {
					return nil, errors.New("invalid range numerical band")
				}
				if _, err := thresholdFor(h, g); err != nil {
					return nil, err
				}
				if err := s.validateRangeGrade(h, v); err != nil {
					return nil, err
				}
				break
			}
			if g.Band != "pipeline-replay-v1" {
				return nil, errors.New("invalid pipeline numerical band")
			}
			break
		}
		if h.Moe != nil {
			if g.Band != "moe-segment-v1" {
				return nil, errors.New("invalid MoE numerical band")
			}
			break
		}
		if _, err := thresholdFor(h, g); err != nil {
			return nil, err
		}
	default:
		return nil, errors.New("unknown offense class")
	}
	return g, nil
}
func thresholdFor(h *Hold, g *pb.OffenseGrade) (*ThresholdPair, error) {
	for _, t := range h.MisjudgeThresholds {
		if t.Model == h.ModelID && t.Band == g.Band && t.LengthClass == g.LengthClass && h.AuditOutputTokens >= t.MinOutput && h.AuditOutputTokens <= t.MaxOutput {
			copy := t
			return &copy, nil
		}
	}
	return nil, errors.New("unknown frozen numerical model/band/length")
}

// classifyOffense is read-only; receipt validation and recording use the same classification.
func (s *State) classifyOffense(h *Hold, node string, height int64) (OffenseRecord, error) {
	g, err := s.validateGrade(h, h.VerdictBytes)
	if err != nil {
		return OffenseRecord{}, err
	}
	r := OffenseRecord{Version: 2, HoldID: h.ID, NodeID: node, Height: height, Class: g.Class, Score: g.Score, Band: g.Band, LengthClass: g.LengthClass, Fault: g.Fault, Status: "slash"}
	if (g.Class == "numerical" || g.Class == "token") && h.FirstGrade != nil && h.FirstGrade.Score < r.Score {
		r.Score = h.FirstGrade.Score
	}
	// v14 spot conviction already requires independent full-review corroboration
	// of the same step. The finalized spot protocol treats this positive evidence
	// as fraud, rather than the legacy uncorroborated numerical gray/refund path.
	if s.Version >= 14 && h.Pipeline != nil && h.Pipeline.Spot != nil && h.Pipeline.Spot.Round == "esc" && h.AuditVerdict == "reject" && h.FirstAuditor != "" && h.FirstAuditor != h.Auditor {
		return r, nil
	}
	p := s.Parameters.Misjudge
	if g.Class == "numerical" && (h.Pipeline == nil && h.Moe == nil || g.Band == core.RangeBand) {
		r.ThetaPair, err = thresholdFor(h, g)
		if err != nil {
			return OffenseRecord{}, err
		}
	}
	// MoE profiles carry their own misjudge governance (moeMisjudgeMode):
	// a profile-level "on" keeps numerical segment offenses slashable
	// under the global gray_gate/refund_only modes, because Plan B's
	// honest-segment fund is built from slash proceeds; a profile-level
	// "off" forces refund-only. The global "off" kill switch still wins.
	moeSlashMode := ""
	if h.Moe != nil {
		moeSlashMode = s.moeMisjudgeMode(h)
	}
	switch {
	case p.SlashMode == "off":
		r.Status = "off"
	case g.Class == "numerical" && moeSlashMode == "off":
		r.Status = "refund-only"
	case g.Class == "numerical" && p.SlashMode == "refund_only" && moeSlashMode != "on":
		r.Status = "refund-only"
	// Pipeline replay has no threshold pair; gray_gate therefore only refunds.
	case g.Class == "numerical" && ((h.Pipeline != nil && g.Band != core.RangeBand) || (h.Moe != nil && moeSlashMode != "on")) && p.SlashMode == "gray_gate":
		r.Status = "refund-only"
	case g.Class == "numerical" && h.Moe != nil && moeSlashMode == "on":
		// Profile governance keeps the offense slashable: fall through
		// with the default "slash" status. (This case must precede the
		// whole-model gray_gate branch below, which dereferences the
		// threshold pair MoE holds do not carry.)
	case g.Class == "numerical" && p.SlashMode == "gray_gate":
		count := uint32(0)
		for _, previous := range s.OffenseRecords {
			if previous.NodeID == node && previous.Class == "numerical" && (previous.Status == "gray" || previous.Status == "pending-review") && height >= previous.Height && height-previous.Height < p.WindowBlocks {
				if count < p.GrayRepeatLimit {
					count++
				}
			}
		}
		if r.Score < r.ThetaPair.Soft {
			r.Status = "below-soft"
		} else if count >= p.GrayRepeatLimit {
			r.Status = "pending-review"
		} else if r.Score < r.ThetaPair.Hard {
			r.Status = "gray"
		}

	}
	return r, nil
}

func (s *State) recordOffense(h *Hold, node string, height int64) (bool, error) {
	r, err := s.classifyOffense(h, node, height)
	if err != nil {
		return false, err
	}
	if r.Status == "gray" || r.Status == "pending-review" {
		if s.GrayObservations == nil {
			s.GrayObservations = map[string]uint64{}
		}
		if s.GrayObservations[node] == ^uint64(0) {
			return false, errors.New("gray observation counter overflow")
		}
		s.GrayObservations[node]++
	}
	s.OffenseRecords = append(s.OffenseRecords, r)
	if r.Status == "pending-review" {
		h.State = "pending-review"
	}
	return r.Status == "slash", nil
}

// moeMisjudgeMode resolves the profile-level misjudge slash mode for a
// MoE hold: the signed layout's misjudge_slash_mode when set; otherwise
// the governance default — "on" for granular settlement (Plan B's
// honest-segment compensation fund is built from slash proceeds, so a
// refund-only default would leave it short), "" (inherit the global
// mode) for atomic settlement.
func (s *State) moeMisjudgeMode(h *Hold) string {
	manifest, _, _, err := s.moeModel(h.ModelID)
	if err != nil || manifest.GetMoeLayout() == nil {
		return ""
	}
	layout := manifest.MoeLayout
	if layout.MisjudgeSlashMode != "" {
		return layout.MisjudgeSlashMode
	}
	if layout.SegmentSettlement != "atomic" {
		return "on"
	}
	return ""
}

func (p MisjudgeParameters) MarshalJSON() ([]byte, error) {
	type alias MisjudgeParameters
	var consumer *ConsumerGrayParameters
	if p.ConsumerGray != (ConsumerGrayParameters{}) {
		v := p.ConsumerGray
		consumer = &v
	}
	return json.Marshal(struct {
		alias
		ConsumerGray *ConsumerGrayParameters `json:"consumer_gray,omitempty"`
	}{alias(p), consumer})
}
