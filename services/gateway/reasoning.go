package gateway

import (
	core "ember/core-go"
	pb "ember/proto"
)

func validReasoning(q request) bool {
	if q.ThinkingEnabled == nil {
		return q.ThinkingBudget == 0 && q.AnswerBudget == 0
	}
	return q.AnswerBudget > 0 && uint64(q.ThinkingBudget)+uint64(q.AnswerBudget) <= uint64(q.MaxTokens) && ((*q.ThinkingEnabled && q.ThinkingBudget > 0) || (!*q.ThinkingEnabled && q.ThinkingBudget == 0))
}
func requestSamplingHash(q request) [32]byte {
	v := uint32(0)
	on := false
	if q.ThinkingEnabled != nil {
		v = 2
		on = *q.ThinkingEnabled
	}
	return core.SamplingParamsHash(v, uint32(q.MaxTokens), on, q.ThinkingBudget, q.AnswerBudget)
}
func setReasoningCommitment(c *pb.ExecutionCommitment, q request) {
	if q.ThinkingEnabled != nil {
		c.SamplingParamsVersion = 2
		c.ThinkingEnabled = *q.ThinkingEnabled
		c.ThinkingBudget = q.ThinkingBudget
		c.AnswerBudget = q.AnswerBudget
	}
}
