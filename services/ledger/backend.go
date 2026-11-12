package ledger

import (
	"time"

	pb "ember/proto"
)

type Backend interface {
	Close() error
	Ping() error
	Create(string, int64) error
	Balance(string) (int64, int64, error)
	IssueToken(string, int64, string) error
	TokenAccount(string) (string, error)
	Stake(string, int64) error
	Freeze(string, string, string, int64) error
	Submit(string, *pb.ExecutionReceipt, int64, time.Duration) error
	Finish(string, string, time.Time, int64) error
	Cancel(string) error
	RecoverFrozen() (int64, error)
	RecordAudit(string, *pb.AuditChallenge, *pb.AuditVerdict) error
	State(string) (string, error)
	RecordedVerdict(string) (*pb.AuditVerdict, error)
	FreezePipeline(string, string, int64, []StageAuthorization, uint32) error
	SubmitPipeline(string, *pb.PipelineExecutionReceipt, int64, uint32, time.Duration) ([]StageShare, error)
	FinishPipeline(string, string, time.Time, int64) error
	CancelPipeline(string) error
	RecordPipelineAudit(string, *pb.AuditChallenge, *pb.AuditVerdict) error
	RecordedPipelineVerdict(string) (*pb.AuditVerdict, error)
	PipelineCredits(string) ([]StageShare, error)
}

func (l *Ledger) Ping() error { return l.db.Ping() }

var _ Backend = (*Ledger)(nil)
