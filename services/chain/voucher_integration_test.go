package chain

import "testing"

func TestBothVoucherSystemsRefundWithoutConversion(t *testing.T) {
	s, a := testState(t)
	id := a["payer"].id
	s.Accounts[id].Balance = 100
	s.Accounts[id].Marked = 30
	s.Accounts[id].ConsumptionCredit = 20
	marked, err := s.debitAllInference(id, 100)
	if err != nil {
		t.Fatal(err)
	}
	h := &Hold{Payer: id, Amount: 100, Marked: marked, SubsidyDebit: s.takeInferenceDebit()}
	if err = s.refundInference(h, 100); err != nil {
		t.Fatal(err)
	}
	got := s.Accounts[id]
	if got.Balance != 100 || got.Marked != 30 || got.ConsumptionCredit != 20 {
		t.Fatalf("refund converted vouchers: %+v", got)
	}
}
