package provenance

import (
	"strings"
	"testing"
)

func TestVersionsCompareNumerically(t *testing.T) {
	if !attestationExpected("v2.10.0", "v2.9.0") || attestationExpected("v2.8.1", "v2.9.0") || !attestationExpected("v2.9.0", "v2.9.0") {
		t.Error("v2.10.0 sorts after v2.9.0, which a string compare gets wrong")
	}
	if !attestationExpected("v1.2.3-rc1", "v1.2.0") || !attestationExpected("1.2.3+build", "v1.2.0") {
		t.Error("prerelease and build suffixes are ignored, the v prefix is optional")
	}
	if attestationExpected("v9.9.9", "") {
		t.Error("no signed release yet means none is expected to carry one")
	}
	if AttestationExpected("v9.9.9") {
		t.Error("the shipped constant says nothing has been signed")
	}
}

func TestIdentitiesPinTheTagAndTheWorkflow(t *testing.T) {
	if got := SignerIdentity(".github/workflows/docker-publish.yml", "v1.2.3"); got != "https://github.com/DinethShakya23/kube-sre/.github/workflows/docker-publish.yml@refs/tags/v1.2.3" {
		t.Errorf("%s", got)
	}
	if SignerWorkflow(".github/workflows/x.yml") != "DinethShakya23/kube-sre/.github/workflows/x.yml" {
		t.Error("no ref in a signer workflow")
	}
}

func TestEveryCommandNamesItsSignerAndDropsTheV(t *testing.T) {
	entries := VerifyCommands("v1.2.3")
	if len(entries) != len(Artifacts) || len(entries) < 3 {
		t.Fatal(len(entries))
	}
	for _, e := range entries {
		if strings.Contains(e.Command, "{version}") {
			t.Errorf("%s: placeholder left in", e.Key)
		}
		if !strings.Contains(e.Command, "--signer-workflow "+SignerWorkflow(e.Workflow)) {
			t.Errorf("%s: dropping --signer-workflow would accept ANY workflow in the repo:\n%s", e.Key, e.Command)
		}
		if strings.Contains(e.Command, ":v1.2.3") {
			t.Errorf("%s: artifact references drop the leading v", e.Key)
		}
		if !strings.HasSuffix(e.Identity, "@refs/tags/v1.2.3") || e.Proves == "" {
			t.Errorf("%+v", e)
		}
	}
	if !strings.Contains(entries[0].Command, ":1.2.3") || !strings.Contains(entries[0].Command, "--predicate-type "+SPDXPredicate) {
		t.Errorf("%s", entries[0].Command)
	}
}

func TestReportSaysUnsignedWithoutSoundingLikeTampering(t *testing.T) {
	r := Report("1.2.3")
	for _, want := range []string{"v1.2.3 is not signed", "No release has been signed yet", "is NOT evidence of tampering", "Not attested, and why", "NOT say the same source rebuilds bit-for-bit", OIDCIssuer} {
		if !strings.Contains(r, want) {
			t.Errorf("missing %q", want)
		}
	}
	if strings.Contains(r, "carries a keyless sigstore attestation") {
		t.Error("must not claim a signature that does not exist")
	}
}
