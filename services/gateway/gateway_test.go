package gateway

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"ember/services/ledger"
)

func TestPromptForSmolLM(t *testing.T) {
	prompt, err := promptFor("ember-smollm2-360m-gpu-v1", []message{{Role: "user", Content: "Say OK"}})
	want := "<|im_start|>system\nYou are a helpful AI assistant named SmolLM, trained by Hugging Face<|im_end|>\n<|im_start|>user\nSay OK<|im_end|>\n<|im_start|>assistant\n"
	if err != nil || prompt != want {
		t.Fatalf("SmolLM tokenizer template mismatch: %q %v", prompt, err)
	}
	prompt, err = promptFor("ember-smollm2-360m-gpu-v1", []message{{Role: "system", Content: "Be brief"}})
	if err != nil || prompt != "<|im_start|>system\nBe brief<|im_end|>\n<|im_start|>assistant\n" {
		t.Fatalf("explicit system prompt mismatch: %q %v", prompt, err)
	}
}

func TestPromptForQwen(t *testing.T) {
	prompt, err := promptFor("ember-qwen2.5-1.5b", []message{{Role: "user", Content: "Say OK"}})
	if err != nil {
		t.Fatal(err)
	}
	want := "<|im_start|>system\nYou are Qwen, created by Alibaba Cloud. You are a helpful assistant.<|im_end|>\n<|im_start|>user\nSay OK<|im_end|>\n<|im_start|>assistant\n"
	if prompt != want {
		t.Fatalf("chat template mismatch: %q", prompt)
	}
	prompt, err = promptFor("ember-qwen2.5-1.5b", []message{{Role: "system", Content: "Act briefly"}, {Role: "user", Content: "Say OK"}})
	if err != nil || prompt != "<|im_start|>system\nAct briefly<|im_end|>\n<|im_start|>user\nSay OK<|im_end|>\n<|im_start|>assistant\n" {
		t.Fatalf("explicit system prompt mismatch: %q, %v", prompt, err)
	}
	pipeline, err := promptFor("ember-qwen2.5-1.5b-pipeline2", []message{{Role: "user", Content: "Say OK"}})
	if err != nil || pipeline != want {
		t.Fatalf("pipeline chat template mismatch: %q, %v", pipeline, err)
	}
}

func TestAdmissionRejectsWithoutQueueing(t *testing.T) {
	book, err := ledger.Open(filepath.Join(t.TempDir(), "gateway.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer book.Close()
	token := strings.Repeat("x", 32)
	if err = book.IssueToken("consumer", 100, token); err != nil {
		t.Fatal(err)
	}
	g := &Gateway{Ledger: book, Admission: make(chan struct{}, 1), Price: 1}
	g.Admission <- struct{}{}
	r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(
		`{"model":"ember-mock","messages":[{"role":"user","content":"x"}],"max_tokens":1}`))
	r.Header.Set("Authorization", "Bearer "+token)
	w := httptest.NewRecorder()
	g.ServeHTTP(w, r)
	if w.Code != http.StatusTooManyRequests || w.Header().Get("Retry-After") == "" {
		t.Fatalf("admission response=%d retry=%q", w.Code, w.Header().Get("Retry-After"))
	}
	if !strings.Contains(g.Metrics(), "ember_gateway_requests_rejected_total 1") {
		t.Fatal(g.Metrics())
	}
}

func TestPromptForMockAndInvalidRole(t *testing.T) {
	prompt, err := promptFor("ember-mock", []message{{Role: "user", Content: "hello"}})
	if err != nil || prompt != "user: hello\n" {
		t.Fatalf("mock prompt changed: %q, %v", prompt, err)
	}
	if _, err := promptFor("ember-qwen2.5-1.5b", []message{{Role: "tool", Content: "no"}}); err == nil {
		t.Fatal("unsupported role accepted")
	}
}
