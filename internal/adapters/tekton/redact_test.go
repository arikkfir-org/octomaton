package tekton

import (
	"context"
	"encoding/base64"
	"errors"
	"strings"
	"testing"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	k8stesting "k8s.io/client-go/testing"
	"octomaton.dev/internal/services/ci"
)

// Fake credentials, built at run time so no scanner mistakes this file for a leak.
var (
	fakeToken   = "ghs_" + strings.Repeat("A1b2", 9)
	fakePAT     = "github_pat_" + strings.Repeat("Z9y8", 15)
	fakeAPIKey  = "sk-" + strings.Repeat("0f", 16)
	fakeSecret  = "correct-horse-battery-staple"
	fakePEM     = "-----BEGIN RSA " + "PRIVATE KEY-----\nMIIEowIBAAKCAQEAx1y2z3\nQ29tcGxldGVseUZha2VLZXlMaW5l\n-----END RSA " + "PRIVATE KEY-----"
	fakeKeyBody = base64.StdEncoding.EncodeToString([]byte("AnotherFakeKeyLineOfBase64"))
)

func b64(s string) string { return base64.StdEncoding.EncodeToString([]byte(s)) }

func jwtPart(s string) string { return base64.RawURLEncoding.EncodeToString([]byte(s)) }

func TestRedact(t *testing.T) {
	pem := fakePEM
	red := newRedactor([][]byte{[]byte(fakeSecret + "\n"), []byte(pem), []byte("short"), []byte("tlsKey\n" + fakeKeyBody)})
	for _, tc := range []struct {
		name, in, want string
	}{
		{"a Secret value", "password is " + fakeSecret + ".", "password is [REDACTED]."},
		{"a Secret value with surrounding whitespace in the Secret", "x" + fakeSecret + "x", "x[REDACTED]x"},
		{"a short Secret value stays", "a short reply", "a short reply"},
		{"one line of a multi-line Secret", "key line: " + fakeKeyBody, "key line: [REDACTED]"},
		{"a whole private key", "before\n" + pem + "\nafter", "before\n[REDACTED]\nafter"},
		{"a base64-encoded Secret value", "data: " + b64(fakeSecret), "data: [REDACTED]"},
		{"a Secret value inside a base64 Basic credential", "extraHeader=x " + b64("x-access-token:"+fakeSecret), "extraHeader=x [REDACTED]"},
		{"a URL-safe base64 Secret value", base64.RawURLEncoding.EncodeToString([]byte("k=" + fakeSecret + "??")), "[REDACTED]"},
		{"a GitHub installation token, in no Secret", "token " + fakeToken + " end", "token [REDACTED] end"},
		{"a fine-grained PAT", fakePAT, "[REDACTED]"},
		{"a GitHub token inside base64", b64("x-access-token:" + fakeToken), "[REDACTED]"},
		{"an sk- API key", "DEEPSEEK_API_KEY=" + fakeAPIKey, "DEEPSEEK_API_KEY=[REDACTED]"},
		{"a Google access token", "ya29." + strings.Repeat("a1", 15), "[REDACTED]"},
		{"a Google API key", "AIza" + strings.Repeat("B", 35), "[REDACTED]"},
		{"a JWT", strings.Join([]string{jwtPart(`{"alg":"RS256"}`), jwtPart(`{"sub":"12345"}`), jwtPart("signature-value")}, "."), "[REDACTED]"},
		{"an AWS access key ID", "AKIA" + strings.Repeat("Q", 16), "[REDACTED]"},
		{"a Slack token", "xoxb-" + strings.Repeat("1", 12), "[REDACTED]"},
		{"an Authorization header keeps its scheme", "> Authorization: Bearer abc.def", "> Authorization: Bearer [REDACTED]"},
		{"URL user information keeps the user", "fatal: https://user:hunter22@example.com/x", "fatal: https://user:[REDACTED]@example.com/x"},
		{"the tail of a private key cut off at the top", "ZmFrZQ==\n-----END PRIVATE KEY-----\nnext", "[REDACTED]\nnext"},
		{"the head of a private key cut off at the bottom", "x\n-----BEGIN PRIVATE KEY-----\nZmFrZQ==", "x\n[REDACTED]"},
		{"ordinary output stays", "ok  \toctomaton.dev/internal/adapters/tekton\t0.2s\nsha256:" + strings.Repeat("ab", 32), "ok  \toctomaton.dev/internal/adapters/tekton\t0.2s\nsha256:" + strings.Repeat("ab", 32)},
		{"ordinary base64 stays", "digest " + b64("nothing to hide here"), "digest " + b64("nothing to hide here")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := red.Redact(tc.in); got != tc.want {
				t.Fatalf("Redact(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestWithholdingRedactor(t *testing.T) {
	red := &redactor{withhold: true}
	for in, want := range map[string]string{"": "", "2 tests failed": withheld, fakeToken: withheld} {
		if got := red.Redact(in); got != want {
			t.Fatalf("Redact(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestRedactorForRun(t *testing.T) {
	h := newRunnerHarness(t)
	ctx := context.Background()
	id := h.create(demoSpec(pushTrigger(shaA)), 1).ID
	tok := ci.Token{Value: "fake logs", ExpiresAt: time.Now().Add(time.Hour)}
	run, err := h.r.Get(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if err := h.r.SetToken(ctx, run, tok); err != nil {
		t.Fatal(err)
	}
	// The fake clientset's logs are always "fake logs": here they are the run's token.
	if logs, err := h.r.StepLogs(ctx, id, ci.Step{Name: "b", Logs: "build-pod/step-b"}, 50, 1024); err != nil || logs != redacted {
		t.Fatalf("StepLogs = %q, %v; want the token redacted", logs, err)
	}

	h.taskRun(id, "build", "build", time.Date(2026, 5, 1, 9, 2, 0, 0, time.UTC), map[string]any{
		"podName": "build-pod", "startTime": "2026-05-01T09:02:00Z", "completionTime": "2026-05-01T09:04:00Z",
		"conditions": []any{map[string]any{"type": "Succeeded", "status": "False", "reason": "Failed", "message": "printed fake logs"}},
		"results":    []any{map[string]any{"name": "check-summary", "value": "leaked " + fakeToken}},
	})
	details, err := h.r.Details(ctx, id)
	if err != nil {
		t.Fatalf("Details: %v", err)
	}
	for _, task := range details.Tasks {
		if task.Name != "build" {
			continue
		}
		if task.Message != "printed "+redacted || task.Results["check-summary"] != "leaked "+redacted {
			t.Fatalf("build task = %+v; want its message and result redacted", task)
		}
	}

	// A Secret that can't be read withholds the logs and details rather than posting them unredacted.
	h.kube.PrependReactor("get", "secrets", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewForbidden(schema.GroupResource{Resource: "secrets"}, "x", errors.New("denied"))
	})
	if _, err := h.r.StepLogs(ctx, id, ci.Step{Name: "b", Logs: "build-pod/step-b"}, 50, 1024); err == nil || !strings.Contains(err.Error(), "to redact") {
		t.Fatalf("StepLogs with an unreadable Secret: %v", err)
	}
	details, err = h.r.Details(ctx, id)
	if err != nil {
		t.Fatalf("Details with an unreadable Secret: %v; the report must still conclude", err)
	}
	for _, task := range details.Tasks {
		if task.Name != "build" {
			continue
		}
		if task.State != ci.TaskFailed || task.Message != withheld || task.Results["check-summary"] != withheld {
			t.Fatalf("build task = %+v; want its state kept and its message and result withheld", task)
		}
	}
}

func TestRedactorForMissingSecret(t *testing.T) {
	h := newRunnerHarness(t)
	id := h.create(demoSpec(pushTrigger(shaA)), 1).ID
	if _, err := h.kube.CoreV1().Secrets(demoNS).Get(context.Background(), tokenSecretName(id.Name), metav1.GetOptions{}); !apierrors.IsNotFound(err) {
		t.Fatalf("the token Secret exists before SetToken: %v", err)
	}
	if logs, err := h.r.StepLogs(context.Background(), id, ci.Step{Name: "b", Logs: "build-pod/step-b"}, 50, 1024); err != nil || logs != "fake logs" {
		t.Fatalf("StepLogs = %q, %v; a missing Secret has nothing to redact", logs, err)
	}
}
