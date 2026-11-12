package chain

import (
	"encoding/hex"
	"errors"
	"math"

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
	SlashMode       string          `json:"slash_mode"`
	GrayRepeatLimit uint32          `json:"gray_repeat_limit"`
	WindowBlocks    int64           `json:"window_blocks"`
	CapBlockBPS     uint32          `json:"cap_block_bps"`
	Thresholds      []ThresholdPair `json:"thresholds,omitempty"`
}

func (p *MisjudgeParameters) defaults() {
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
	if p.GrayRepeatLimit == 0 || p.WindowBlocks < 1 || p.CapBlockBPS == 0 || p.CapBlockBPS > 10000 {
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
		case "signature", "hash", "format":
		default:
			return nil, errors.New("unknown objective fault (N_in grading unavailable)")
		}
	case "numerical":
		if g.Fault != "" {
			return nil, errors.New("numerical offense cannot claim objective fault")
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

func (s *State) recordOffense(h *Hold, node string, height int64) (bool, error) {
	g, err := s.validateGrade(h, h.VerdictBytes)
	if err != nil {
		return false, err
	}
	r := OffenseRecord{Version: 2, HoldID: h.ID, NodeID: node, Height: height, Class: g.Class, Score: g.Score, Band: g.Band, LengthClass: g.LengthClass, Fault: g.Fault, Status: "slash"}
	if g.Class == "numerical" && h.FirstGrade != nil && h.FirstGrade.Score < r.Score {
		r.Score = h.FirstGrade.Score
	}
	p := s.Parameters.Misjudge
	if g.Class == "numerical" {
		r.ThetaPair, err = thresholdFor(h, g)
		if err != nil {
			return false, err
		}
	}
	switch {
	case p.SlashMode == "off":
		r.Status = "off"
	case g.Class == "numerical" && p.SlashMode == "refund_only":
		r.Status = "refund-only"
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
