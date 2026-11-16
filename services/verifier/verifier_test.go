package verifier

import (
 "crypto/ed25519"
 "testing"
 "time"
 core "ember/core-go"
 pb "ember/proto"
)

func TestDraw(t *testing.T) {
 seed:=[32]byte{1};key:=ed25519.NewKeyFromSeed(seed[:])
 receipt:=&pb.ExecutionReceipt{RequestId:"r",OutputTokens:33}
 if err:=core.Sign(receipt,key);err!=nil{t.Fatal(err)}
 c,drawn,err:=Draw(receipt,key,100,time.Now());if err!=nil||!drawn{t.Fatalf("draw: %v",err)}
 if err:=CheckDraw(c,receipt);err!=nil{t.Fatal(err)}
 c.Positions[0]=2
 if err:=CheckDraw(c,receipt);err==nil{t.Fatal("accepted modified challenge")}
 c,drawn,err=Draw(receipt,key,0,time.Now());if err!=nil||drawn||c!=nil {t.Fatal("zero probability drew")}
}
